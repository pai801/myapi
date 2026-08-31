package model

import (
	"testing"

	"github.com/pai801/myapi/common"
	. "github.com/smartystreets/goconvey/convey"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// initTestTokenDB sets up an in-memory SQLite DB with one enabled token.
// Redis is left disabled so cache functions fall through to DB queries.
func initTestTokenDB(t *testing.T) *Token {
	t.Helper()
	common.RedisEnabled = false
	common.UsingSQLite = true

	var err error
	DB, err = gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open in-memory db: %v", err)
	}
	_ = DB.AutoMigrate(&Token{})

	token := &Token{
		UserId: 1,
		Key:    "test-token-key-000000000000000000000000",
		Status: TokenStatusEnabled,
	}
	if err := DB.Create(token).Error; err != nil {
		t.Fatalf("failed to create token: %v", err)
	}
	return token
}

func TestTokenCacheInvalidationAfterWrite(t *testing.T) {
	Convey("Given one enabled token, after write paths the cached view reflects changes immediately", t, func() {
		token := initTestTokenDB(t)

		Convey("Update disabling the token takes effect immediately", func() {
			token.Status = TokenStatusDisabled
			err := token.Update()
			So(err, ShouldBeNil)

			_, verr := ValidateUserToken(token.Key)
			So(verr, ShouldNotBeNil)
			So(verr.Error(), ShouldEqual, "该令牌状态不可用")
		})

		Convey("Update re-enabling the token takes effect immediately", func() {
			token.Status = TokenStatusDisabled
			So(token.Update(), ShouldBeNil)

			token.Status = TokenStatusEnabled
			So(token.Update(), ShouldBeNil)

			_, verr := ValidateUserToken(token.Key)
			So(verr, ShouldBeNil)
		})

		Convey("Delete takes effect immediately", func() {
			err := token.Delete()
			So(err, ShouldBeNil)

			_, verr := ValidateUserToken(token.Key)
			So(verr, ShouldNotBeNil)
			So(verr.Error(), ShouldEqual, "无效的令牌")
		})
	})
}

func TestSimplifyModelsField(t *testing.T) {
	Convey("SimplifyModelsField converts model names to aliases", t, func() {
		Convey("simplifies multiple models", func() {
			models := "gpt-4-turbo,gpt-3.5-turbo"
			result := SimplifyModelsField(&models)
			So(*result, ShouldEqual, "gpt4turbo,gpt35turbo")
		})

		Convey("simplifies empty string", func() {
			models := ""
			result := SimplifyModelsField(&models)
			So(*result, ShouldEqual, "")
		})

		Convey("returns nil for nil input", func() {
			result := SimplifyModelsField(nil)
			So(result, ShouldBeNil)
		})
	})
}