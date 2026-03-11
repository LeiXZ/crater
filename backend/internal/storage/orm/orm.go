package orm

import (
	"fmt"
	"sync"

	"github.com/raids-lab/crater/internal/storage/config"
	"github.com/raids-lab/crater/internal/storage/logutils"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var instance *gorm.DB = nil
var once sync.Once

type FilePermission int

const (
	NotAllowed FilePermission = 0
	ReadOnly
	ReadWrite
)

type GormDBWrapper struct {
	*gorm.DB
}

func opendb() *gorm.DB {
	once.Do(func() {
		if instance == nil {
			dbConfig := config.GetConfig()
			dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=%s TimeZone=%s",
				dbConfig.Postgres.Host, dbConfig.Postgres.User, dbConfig.Postgres.Password,
				dbConfig.Postgres.DBName, dbConfig.Postgres.Port,
				dbConfig.Postgres.SSLMode, dbConfig.Postgres.TimeZone)
			var err error
			instance, err = gorm.Open(postgres.Open(dsn))
			if err != nil {
				logutils.Log.Fatalf("connect to postgres")
				instance = nil
			}
		}
	})
	return instance
}

func DB() *gorm.DB {
	ans := opendb()
	if ans == nil {
		logutils.Log.Fatalf("connect to postgres")
	}
	return ans
}
