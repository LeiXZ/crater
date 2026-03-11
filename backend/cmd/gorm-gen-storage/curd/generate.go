// Description: 生成所有表的 Model 结构体和 CRUD 代码
package main

import (
	"github.com/raids-lab/crater/internal/storage/dao/model"
	"github.com/raids-lab/crater/internal/storage/dao/query"

	"gorm.io/gen"
)

func main() {
	err := query.InitDB()
	if err != nil {
		panic(err)
	}

	g := gen.NewGenerator(gen.Config{
		OutPath: "./internal/storage/dao/query",

		// gen.WithoutContext：禁用WithContext模式
		// gen.WithDefaultQuery：生成一个全局Query对象Q
		// gen.WithQueryInterface：生成Query接口
		Mode: gen.WithDefaultQuery | gen.WithQueryInterface,
	})

	// 通常复用项目中已有的SQL连接配置 db(*gorm.DB)
	g.UseDB(query.DB)

	// 从连接的数据库为所有表生成 Model 结构体和 CRUD 代码
	g.ApplyBasic(
		model.User{},
		model.Account{},
		model.UserAccount{},
		model.Dataset{},
		model.AccountDataset{},
		model.UserDataset{},
	)

	// 执行并生成代码
	g.Execute()
}
