/*
 * Copyright (c) 2021 yedf. All rights reserved.
 * Use of this source code is governed by a BSD-style
 * license that can be found in the LICENSE file.
 */

package common

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // register mysql driver
	_ "github.com/lib/pq"              // register postgres driver
	"gopkg.in/yaml.v2"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/yedf/dtm/dtmcli"
	"github.com/yedf/dtm/dtmcli/dtmimp"
)

// ModelBase model base for gorm to provide base fields
type ModelBase struct {
	ID         uint64
	CreateTime *time.Time `gorm:"autoCreateTime"`
	UpdateTime *time.Time `gorm:"autoUpdateTime"`
}

func getGormDialetor(driver string, dsn string) gorm.Dialector {
	if driver == dtmcli.DBTypePostgres {
		return postgres.Open(dsn)
	}
	dtmimp.PanicIf(driver != dtmcli.DBTypeMysql, fmt.Errorf("unkown driver: %s", driver))
	return mysql.Open(dsn)
}

var dbs sync.Map

// DB provide more func over gorm.DB
type DB struct {
	*gorm.DB
}

// Must set must flag, panic when error occur
func (m *DB) Must() *DB {
	db := m.InstanceSet("ivy.must", true)
	return &DB{DB: db}
}

// NoMust unset must flag, don't panic when error occur
func (m *DB) NoMust() *DB {
	db := m.InstanceSet("ivy.must", false)
	return &DB{DB: db}
}

// ToSQLDB get the sql.DB
func (m *DB) ToSQLDB() *sql.DB {
	d, err := m.DB.DB()
	dtmimp.E2P(err)
	return d
}

type tracePlugin struct{}

func (op *tracePlugin) Name() string {
	return "tracePlugin"
}

func (op *tracePlugin) Initialize(db *gorm.DB) (err error) {
	before := func(db *gorm.DB) {
		db.InstanceSet("ivy.startTime", time.Now())
	}

	after := func(db *gorm.DB) {
		_ts, _ := db.InstanceGet("ivy.startTime")
		sql := db.Dialector.Explain(db.Statement.SQL.String(), db.Statement.Vars...)
		dtmimp.Logf("used: %d ms affected: %d sql is: %s", time.Since(_ts.(time.Time)).Milliseconds(), db.RowsAffected, sql)
		if v, ok := db.InstanceGet("ivy.must"); ok && v.(bool) {
			if db.Error != nil && db.Error != gorm.ErrRecordNotFound {
				panic(db.Error)
			}
		}
	}

	beforeName := "cb_before"
	afterName := "cb_after"

	dtmimp.Logf("installing db plugin: %s", op.Name())
	// 开始前
	_ = db.Callback().Create().Before("gorm:before_create").Register(beforeName, before)
	_ = db.Callback().Query().Before("gorm:query").Register(beforeName, before)
	_ = db.Callback().Delete().Before("gorm:before_delete").Register(beforeName, before)
	_ = db.Callback().Update().Before("gorm:setup_reflect_value").Register(beforeName, before)
	_ = db.Callback().Row().Before("gorm:row").Register(beforeName, before)
	_ = db.Callback().Raw().Before("gorm:raw").Register(beforeName, before)

	// 结束后
	_ = db.Callback().Create().After("gorm:after_create").Register(afterName, after)
	_ = db.Callback().Query().After("gorm:after_query").Register(afterName, after)
	_ = db.Callback().Delete().After("gorm:after_delete").Register(afterName, after)
	_ = db.Callback().Update().After("gorm:after_update").Register(afterName, after)
	_ = db.Callback().Row().After("gorm:row").Register(afterName, after)
	_ = db.Callback().Raw().After("gorm:raw").Register(afterName, after)
	return
}

// DbGet get db connection for specified conf
func DbGet(conf map[string]string) *DB {
	dsn := dtmimp.GetDsn(conf)
	db, ok := dbs.Load(dsn)
	if !ok {
		dtmimp.Logf("connecting %s", strings.Replace(dsn, conf["password"], "****", 1))
		db1, err := gorm.Open(getGormDialetor(conf["driver"], dsn), &gorm.Config{
			SkipDefaultTransaction: true,
		})
		dtmimp.E2P(err)
		db1.Use(&tracePlugin{})
		db = &DB{DB: db1}
		dbs.Store(dsn, db)
	}
	return db.(*DB)
}

type dtmConfigType struct {
	TransCronInterval int64             `yaml:"TransCronInterval"`
	TimeoutToFail     int64             `yaml:"TimeoutToFail"`
	RetryInterval     int64             `yaml:"RetryInterval"`
	DB                map[string]string `yaml:"DB"`
	DisableLocalhost  int64             `yaml:"DisableLocalhost"`
	UpdateBranchSync  int64             `yaml:"UpdateBranchSync"`
}

// DtmConfig 配置
var DtmConfig = dtmConfigType{}

func getIntEnv(key string, defaultV string) int64 {
	return int64(dtmimp.MustAtoi(dtmimp.OrString(os.Getenv(key), defaultV)))
}

func init() {
	if len(os.Args) == 1 || os.Args[1] == "version" {
		return
	}
	DtmConfig.TransCronInterval = getIntEnv("TRANS_CRON_INTERVAL", "3")
	DtmConfig.TimeoutToFail = getIntEnv("TIMEOUT_TO_FAIL", "35")
	DtmConfig.RetryInterval = getIntEnv("RETRY_INTERVAL", "10")
	DtmConfig.DB = map[string]string{
		"driver":   dtmimp.OrString(os.Getenv("DB_DRIVER"), "mysql"),
		"host":     os.Getenv("DB_HOST"),
		"port":     dtmimp.OrString(os.Getenv("DB_PORT"), "3306"),
		"user":     os.Getenv("DB_USER"),
		"password": os.Getenv("DB_PASSWORD"),
	}
	DtmConfig.DisableLocalhost = getIntEnv("DISABLE_LOCALHOST", "0")
	DtmConfig.UpdateBranchSync = getIntEnv("UPDATE_BRANCH_SYNC", "0")
	cont := []byte{}
	for d := MustGetwd(); d != "" && d != "/"; d = filepath.Dir(d) {
		cont1, err := ioutil.ReadFile(d + "/conf.yml")
		if err != nil {
			cont1, err = ioutil.ReadFile(d + "/conf.sample.yml")
		}
		if cont1 != nil {
			cont = cont1
			break
		}
	}
	if len(cont) != 0 {
		dtmimp.Logf("config is: \n%s", string(cont))
		err := yaml.Unmarshal(cont, &DtmConfig)
		dtmimp.FatalIfError(err)
	}
	errStr := checkConfig()
	dtmimp.LogIfFatalf(errStr != "",
		`config error: '%s'.
check you env, and conf.yml/conf.sample.yml in current and parent path: %s.
please visit http://d.dtm.pub to see the config document.
loaded config is:
%v`, MustGetwd(), DtmConfig)
}

func checkConfig() string {
	if DtmConfig.DB["driver"] == "" {
		return "db driver empty"
	} else if DtmConfig.DB["user"] == "" || DtmConfig.DB["host"] == "" {
		return "db config not valid"
	} else if DtmConfig.RetryInterval < 10 {
		return "RetryInterval should not be less than 10"
	} else if DtmConfig.TimeoutToFail < DtmConfig.RetryInterval {
		return "TimeoutToFail should not be less than RetryInterval"
	}
	return ""
}

// WaitDBUp wait for db to go up
func WaitDBUp() {
	sdb, err := dtmimp.StandaloneDB(DtmConfig.DB)
	dtmimp.FatalIfError(err)
	defer func() {
		sdb.Close()
	}()
	for _, err := dtmimp.DBExec(sdb, "select 1"); err != nil; { // wait for mysql to start
		time.Sleep(3 * time.Second)
		_, err = dtmimp.DBExec(sdb, "select 1")
	}
}
