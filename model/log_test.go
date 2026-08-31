package model

import (
	"testing"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestLogDetailFields(t *testing.T) {
	Convey("Log struct should have detail fields", t, func() {
		l := Log{
			ChannelName:   "test-channel",
			RequestBody:   "{\"key\":\"value\"}",
			ResponseBody:  "{\"result\":\"ok\"}",
			RequestHeader: "Content-Type: application/json",
		}
		So(l.ChannelName, ShouldEqual, "test-channel")
		So(l.RequestBody, ShouldEqual, "{\"key\":\"value\"}")
		So(l.ResponseBody, ShouldEqual, "{\"result\":\"ok\"}")
		So(l.RequestHeader, ShouldEqual, "Content-Type: application/json")
	})
}

// initTestLogDB sets up an in-memory SQLite logs DB with two seeded rows.
// Redis is left disabled so cache functions fall through to DB queries.
func initTestLogDB(t *testing.T) {
	t.Helper()
	common.RedisEnabled = false
	common.UsingSQLite = true

	var err error
	LOG_DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory log db: %v", err)
	}
	_ = LOG_DB.AutoMigrate(&Log{})

	seed := []*Log{
		{UserId: 1, Username: "u1", Type: LogTypeConsume, Content: "abc-def", CreatedAt: 100},
		{UserId: 1, Username: "u1", Type: LogTypeManage, Content: "quota changed", CreatedAt: 101},
	}
	for _, l := range seed {
		if err := LOG_DB.Create(l).Error; err != nil {
			t.Fatalf("failed to seed log: %v", err)
		}
	}
}

// keyword 搜索修复回归：非数字 keyword 不得与整数 type 比较（PG 直接报错、
// MySQL 隐式转型为 0 语义错误）；SearchUserLogs 必须按 content 搜索本用户日志
func TestLogSearchKeyword(t *testing.T) {
	Convey("Given two seeded logs, keyword search filters safely", t, func() {
		initTestLogDB(t)

		Convey("Non-numeric keyword filters by content prefix only", func() {
			found, err := SearchAllLogs("abc")
			So(err, ShouldBeNil)
			So(len(found), ShouldEqual, 1)
			So(found[0].Content, ShouldEqual, "abc-def")
		})

		Convey("Numeric keyword still matches log type", func() {
			found, err := SearchAllLogs("3")
			So(err, ShouldBeNil)
			So(len(found), ShouldEqual, 1)
			So(found[0].Type, ShouldEqual, LogTypeManage)
		})

		Convey("SearchUserLogs searches content of the given user", func() {
			found, err := SearchUserLogs(1, "quota")
			So(err, ShouldBeNil)
			So(len(found), ShouldEqual, 1)
			So(found[0].Content, ShouldEqual, "quota changed")

			other, err := SearchUserLogs(2, "quota")
			So(err, ShouldBeNil)
			So(len(other), ShouldEqual, 0)
		})
	})
}
