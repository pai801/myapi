package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/model"
	. "github.com/smartystreets/goconvey/convey"
)

// TestEscapeLike 表驱动守护转义纯函数：% _ \ 均须前置 \，且 \ 自身最先转义，
// 否则后续追加的 \ 会被二次转义导致 ESCAPE '\' 语义错误
func TestEscapeLike(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "abc", "abc"},
		{"empty", "", ""},
		{"percent", "a%b", `a\%b`},
		{"underscore", "a_b", `a\_b`},
		{"backslash", `a\b`, `a\\b`},
		{"mixed", `100%\_`, `100\%\\\_`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeLike(tc.in); got != tc.want {
				t.Fatalf("escapeLike(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// likeEscapeQuery 按 DeleteGroup/UpdateGroup 同款结构执行 LIKE+ESCAPE 查询，
// 返回命中的 Channel.Group 值（排序后便于比较）
func likeEscapeQuery(t *testing.T, name string) []string {
	t.Helper()
	escapedName := escapeLike(name)
	var channels []*model.Channel
	err := model.DB.Where(
		"`group` = ? OR `group` LIKE ? ESCAPE '\\' OR `group` LIKE ? ESCAPE '\\' OR `group` LIKE ? ESCAPE '\\'",
		name, escapedName+",%", "%,"+escapedName+",%", "%,"+escapedName,
	).Find(&channels).Error
	if err != nil {
		t.Fatalf("query channels: %v", err)
	}
	groups := make([]string, 0, len(channels))
	for _, ch := range channels {
		groups = append(groups, ch.Group)
	}
	sort.Strings(groups)
	return groups
}

func seedGroupChannel(t *testing.T, group string) {
	t.Helper()
	ch := &model.Channel{
		Name:   "ch-" + group,
		Status: model.ChannelStatusEnabled,
		Group:  group,
		Models: "m1",
		Type:   1,
		Key:    "sk-test",
	}
	if err := model.DB.Create(ch).Error; err != nil {
		t.Fatalf("seed channel: %v", err)
	}
}

// DB 层守护 LIKE+ESCAPE 查询语义：组名含 % _ 时通配符不得被当模式展开，
// 只允许按字面命中真实引用（注意 DeleteGroup 对命中结果还有二次精确校验，
// 故此处直接断言查询命中集合而非 handler 返回，才能暴露通配符误展开）
func TestDeleteGroupLikeEscapeSemantics(t *testing.T) {
	Convey("组名 a%b 只按字面命中引用，不把 % 当通配符展开", t, func() {
		initUpdateUserTestDB(t)
		// 真引用覆盖四个分支：精确、首位、中间、末位；假匹配覆盖通配符可展开的各位置
		for _, g := range []string{"a%b", "a%b,x", "x,a%b,y", "default,a%b", "axb,default", "axb", "xa%b"} {
			seedGroupChannel(t, g)
		}

		So(likeEscapeQuery(t, "a%b"), ShouldResemble, []string{"a%b", "a%b,x", "default,a%b", "x,a%b,y"})
	})

	Convey("组名 v1_0 只按字面命中引用，不把 _ 当通配符展开", t, func() {
		initUpdateUserTestDB(t)
		for _, g := range []string{"v1_0", "v1_0,x", "default,v1_0", "v1x0,default", "v1x0"} {
			seedGroupChannel(t, g)
		}

		So(likeEscapeQuery(t, "v1_0"), ShouldResemble, []string{"default,v1_0", "v1_0", "v1_0,x"})
	})
}

type deleteGroupResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// invokeDeleteGroup 走真实 DeleteGroup handler，仅设置路径参数 id
func invokeDeleteGroup(t *testing.T, id int) deleteGroupResponse {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/group/"+strconv.Itoa(id), nil)
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(id)}}
	DeleteGroup(c)
	var resp deleteGroupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse delete group response: %v, body: %s", err, w.Body.String())
	}
	return resp
}

// handler 层回归：含通配符组名的删除路径不被转义修复破坏——
// 字面真引用仍拒绝删除，无字面引用（即使 LIKE 未转义时会多查）仍正常放行
func TestDeleteGroupWithWildcardName(t *testing.T) {
	Convey("字面引用 a%b,x 拒绝删除", t, func() {
		initUpdateUserTestDB(t)
		g := &model.Group{Name: "a%b"}
		So(model.AddGroup(g), ShouldBeNil)
		seedGroupChannel(t, "a%b,x")

		resp := invokeDeleteGroup(t, g.Id)
		So(resp.Success, ShouldBeFalse)
		So(resp.Message, ShouldContainSubstring, "Channel 引用")
	})

	Convey("仅有 axb,default 引用时不误拒删除", t, func() {
		initUpdateUserTestDB(t)
		g := &model.Group{Name: "a%b"}
		So(model.AddGroup(g), ShouldBeNil)
		seedGroupChannel(t, "axb,default")

		resp := invokeDeleteGroup(t, g.Id)
		So(resp.Success, ShouldBeTrue)
		_, err := model.GetGroupByName("a%b")
		So(err, ShouldNotBeNil)
	})
}
