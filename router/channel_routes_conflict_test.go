package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/relay/routeregistry"
)

// routeSpec 描述一条待声明路由（method + 组内相对路径 + handler）。
type routeSpec struct {
	method  string
	path    string
	handler gin.HandlerFunc
}

// specRegistrar 是按需声明路由的假注册方；name / log 仅用于断言处理顺序。
type specRegistrar struct {
	name   string
	public []routeSpec
	auth   []routeSpec
	log    *[]string
}

func (s *specRegistrar) RegisterPublicRoutes(g *gin.RouterGroup) {
	if s.log != nil {
		*s.log = append(*s.log, s.name)
	}
	for _, rs := range s.public {
		g.Handle(rs.method, rs.path, rs.handler)
	}
}

func (s *specRegistrar) RegisterAuthRoutes(g *gin.RouterGroup) {
	for _, rs := range s.auth {
		g.Handle(rs.method, rs.path, rs.handler)
	}
}

func statusHandler(code int) gin.HandlerFunc {
	return func(c *gin.Context) { c.Status(code) }
}

// TestMountChannelRoutesWithEngine_DuplicateBetweenRegistrars 锁定：两个注册方声明相同
// method+path 时，第二个被跳过（不挂载、不 panic），路由只挂一次且保留先到者的 handler。
func TestMountChannelRoutesWithEngine_DuplicateBetweenRegistrars(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	first := &specRegistrar{public: []routeSpec{{http.MethodGet, "/dup", statusHandler(http.StatusCreated)}}}
	second := &specRegistrar{public: []routeSpec{{http.MethodGet, "/dup", statusHandler(http.StatusAccepted)}}}

	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{first, second})

	count := 0
	for _, ri := range r.Routes() {
		if ri.Method == http.MethodGet && ri.Path == "/api/dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("GET /api/dup registered %d times, want 1", count)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/dup", nil))
	if w.Code != http.StatusCreated {
		t.Errorf("GET /api/dup code=%d, want %d (first registrar's handler must win)", w.Code, http.StatusCreated)
	}
}

// TestMountChannelRoutesWithEngine_ConflictWithBuiltinSkipped 锁定：注册方声明的路由与
// 内置路由撞 method+path 时被跳过，不 panic，内置路由仍可用（不被扩展实现顶掉）。
func TestMountChannelRoutesWithEngine_ConflictWithBuiltinSkipped(t *testing.T) {
	r := newTestEngine()
	// 先注册一条「内置」路由，模拟 api.go 里已挂好的内置端点。
	r.GET("/api/builtin/x", statusHandler(http.StatusTeapot))

	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	reg := &specRegistrar{public: []routeSpec{{http.MethodGet, "/builtin/x", statusHandler(http.StatusOK)}}}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{reg})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/builtin/x", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("GET /api/builtin/x code=%d, want %d (built-in route must survive)", w.Code, http.StatusTeapot)
	}
}

// TestMountChannelRoutesWithEngine_DifferentPrefixesNotConflict 锁定：公开组 /api/x 与
// 鉴权组 /api/channel/x 路径不同，不算冲突，两条都能挂上。
func TestMountChannelRoutesWithEngine_DifferentPrefixesNotConflict(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	reg := &specRegistrar{
		public: []routeSpec{{http.MethodGet, "/x", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodGet, "/x", statusHandler(http.StatusOK)}},
	}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{reg})

	got := routeSet(r)
	for _, w := range []string{"GET /api/x", "GET /api/channel/x"} {
		if !got[w] {
			t.Errorf("route %q missing: distinct prefixes must not be treated as conflict", w)
		}
	}
}

// TestMountChannelRoutes_NoConflictKeepsBehaviour 锁定：无冲突时行为与改造前一致——
// 路由可达，且按 registrars 顺序逐个处理（先试探后挂载，故各注册方各出现两次）。
func TestMountChannelRoutes_NoConflictKeepsBehaviour(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	var order []string
	a := &specRegistrar{
		name:   "A",
		public: []routeSpec{{http.MethodGet, "/a", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodPost, "/a", statusHandler(http.StatusOK)}},
		log:    &order,
	}
	b := &specRegistrar{
		name:   "B",
		public: []routeSpec{{http.MethodGet, "/b", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodPost, "/b", statusHandler(http.StatusOK)}},
		log:    &order,
	}

	// 走兼容路径（nil engine），等价于改造前的调用方式。
	mountChannelRoutes(apiRouter, authGroup, []routeregistry.RouteRegistrar{a, b})

	got := routeSet(r)
	for _, w := range []string{
		"GET /api/a", "POST /api/channel/a",
		"GET /api/b", "POST /api/channel/b",
	} {
		if !got[w] {
			t.Errorf("route %q missing after no-conflict mount", w)
		}
	}

	want := []string{"A", "A", "B", "B"}
	if len(order) != len(want) {
		t.Fatalf("registrar invocation order=%v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("registrar invocation order=%v, want %v", order, want)
		}
	}
}

// TestMountChannelRoutesWithEngine_PanickingRegistrarSkipped 锁定：注册方在试探期 panic
// 时被跳过，不影响其它注册方与后续路由。
func TestMountChannelRoutesWithEngine_PanickingRegistrarSkipped(t *testing.T) {
	r := newTestEngine()
	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	good := &specRegistrar{
		public: []routeSpec{{http.MethodGet, "/good", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodPost, "/good", statusHandler(http.StatusOK)}},
	}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{panicRegistrar{}, good})

	got := routeSet(r)
	for _, w := range []string{"GET /api/good", "POST /api/channel/good"} {
		if !got[w] {
			t.Errorf("route %q missing: panicking registrar must not block others", w)
		}
	}
}

// TestMountChannelRoutesWithEngine_ConflictWithBuiltinAuthGroup 覆盖鉴权组这一侧：注册方在
// 鉴权组声明 /builtin/y（落到 /api/channel/builtin/y），与同名内置路由撞 method+path 时，
// 内置路由存活、注册方被跳过——与公开组一侧的 ConflictWithBuiltinSkipped 对称。
//
// 注意本用例只断言「内置路由存活」这一可观测结果，而该结果在「检测到并跳过」与「挂载时
// panic 被 safeMountRegistrar 的 recover 兜住」两种实现下完全相同，因此它**不**锁定前缀是否
// 取自真实分组（实测把 authPrefix 故意传成 publicGroup.BasePath()，本用例仍会通过）。
// 「前缀取自真实分组」由 TestMountChannelRoutesWithEngine_PrefixesFromGroupBasePath 经真实
// 调用点断言——它才是守护 channel_routes.go 调用点是否用 BasePath() 的用例；而
// TestDeclaredRoutes_UsesRealGroupPrefixes 传字面量、绕过了调用点，只锁定 DeclaredRoutes
// 内部把 publicPrefix / authPrefix 分别映射到两个组这一层。
func TestMountChannelRoutesWithEngine_ConflictWithBuiltinAuthGroup(t *testing.T) {
	r := newTestEngine()
	r.POST("/api/channel/builtin/y", statusHandler(http.StatusTeapot))

	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	reg := &specRegistrar{auth: []routeSpec{{http.MethodPost, "/builtin/y", statusHandler(http.StatusOK)}}}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{reg})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/channel/builtin/y", nil))
	if w.Code != http.StatusTeapot {
		t.Errorf("POST /api/channel/builtin/y code=%d, want %d (built-in route must survive)", w.Code, http.StatusTeapot)
	}
}

// TestMountChannelRoutesWithEngine_CoexistWithBuiltins 锁定：engine 非空、含若干内置路由
// （含参数路由），注册方与之不冲突时，内置与注册方路由全部共存、互不干扰。
func TestMountChannelRoutesWithEngine_CoexistWithBuiltins(t *testing.T) {
	r := newTestEngine()
	r.GET("/api/builtin/a", statusHandler(http.StatusOK))
	r.GET("/api/token/:id", statusHandler(http.StatusOK)) // 参数路由，模拟内置

	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	reg := &specRegistrar{
		public: []routeSpec{{http.MethodGet, "/x", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodPost, "/y", statusHandler(http.StatusOK)}},
	}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{reg})

	got := routeSet(r)
	for _, w := range []string{
		"GET /api/builtin/a", "GET /api/token/:id",
		"GET /api/x", "POST /api/channel/y",
	} {
		if !got[w] {
			t.Errorf("route %q missing: non-conflicting registrar must coexist with builtins", w)
		}
	}
}

// lateConflictRegistrar 声明的路由与「在挂载点之后才注册的内置路由」冲突（/api/token/search）。
// 用于锁定修复 2：挂载点移到 SetApiRouter 末尾后，内置基线已含全部内置路由，该冲突能被
// 检测到并跳过；若仍把挂载点放在 token 路由之前，冲突检测不到，gin 会在 token 路由注册
// 阶段触发重复注册 panic，且该 panic 不在 safeMountRegistrar 的 recover 覆盖内，会带崩启动。
type lateConflictRegistrar struct{}

func (lateConflictRegistrar) RegisterPublicRoutes(g *gin.RouterGroup) {
	g.GET("/token/search", statusHandler(http.StatusOK))
}

func (lateConflictRegistrar) RegisterAuthRoutes(*gin.RouterGroup) {}

// TestSetApiRouter_LateBuiltinConflictSkipped 锁定修复 2：注册方声明 /api/token/search
// （该内置路由在原挂载点之后才注册）时，SetApiRouter 不 panic，内置路由只注册一次。
func TestSetApiRouter_LateBuiltinConflictSkipped(t *testing.T) {
	// 注册进默认注册表；幂等处理，避免 -count>1 时 duplicate id panic。
	const id = "test-late-builtin-conflict"
	known := false
	for _, got := range routeregistry.RegisteredIDs() {
		if got == id {
			known = true
			break
		}
	}
	if !known {
		routeregistry.Register(id, lateConflictRegistrar{})
	}

	r := newTestEngine()
	SetApiRouter(r) // 修复前：此处会因 gin 重复注册 GET /api/token/search 而 panic

	count := 0
	for _, ri := range r.Routes() {
		if ri.Method == http.MethodGet && ri.Path == "/api/token/search" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("GET /api/token/search registered %d times, want 1 (built-in must survive, registrar skipped)", count)
	}
}

// TestMountChannelRoutesWithEngine_ConflictSkipsWholeRegistrarNotRecoverFallback 区分「冲突检测
// 生效并整方跳过」与「仅靠 panic recover 兜底」这两种会产生相同可观测结果（不崩、内置路由存活）
// 的实现。
//
// 构造的注册方**按声明顺序**先声明一条不冲突路由 /clean、再声明一条与内置路由冲突的路由
// /builtin/conflict（顺序是关键）：
//   - 检测生效时：整方被跳过 → 两条路由都没挂 → 请求 /api/clean 得到 404；
//   - 只有 recover 兜底时：注册方被真实挂载，/clean 先挂成功，随后冲突路由触发 gin 重复注册
//     panic 被 safeMountRegistrar 的 recover 兜住 → /api/clean 仍命中 handler（非 404）。
//
// 故断言「不冲突的 /api/clean 返回 404」即可把两者区分开：若有人删弱/删掉跳过逻辑，本用例变红。
func TestMountChannelRoutesWithEngine_ConflictSkipsWholeRegistrarNotRecoverFallback(t *testing.T) {
	r := newTestEngine()
	// 内置路由：注册方的第二条声明与之撞 method+path。
	r.GET("/api/builtin/conflict", statusHandler(http.StatusTeapot))

	apiRouter := r.Group("/api")
	authGroup := apiRouter.Group("/channel")

	reg := &specRegistrar{public: []routeSpec{
		// 声明顺序不可调换：不冲突的在前，冲突的在后。
		{http.MethodGet, "/clean", statusHandler(http.StatusOK)},
		{http.MethodGet, "/builtin/conflict", statusHandler(http.StatusOK)},
	}}
	mountChannelRoutesWithEngine(r, apiRouter, authGroup, []routeregistry.RouteRegistrar{reg})

	// 检测生效 → 整方跳过 → /api/clean 不存在 → 404。
	// 仅 recover 兜底 → /api/clean 已挂载（先于 panic）→ 非 404。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/clean", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /api/clean code=%d, want %d: a registrar with any conflicting route must be skipped as a whole (detection active), not partially mounted before a recovered panic", w.Code, http.StatusNotFound)
	}
}

// TestDeclaredRoutes_UsesRealGroupPrefixes 直接调用 DeclaredRoutes 并传入字面量前缀
// （"/api"、"/api/channel"），因此**只**锁定 DeclaredRoutes 内部把 publicPrefix / authPrefix
// 分别映射到公开组、鉴权组的正确性：若实现把某个前缀传错（例如两个都传 publicPrefix），读回的
// 路径就会与预期不符，本用例变红。
//
// 注意：本用例传的是字面量、**绕过了调用点**，故并不覆盖 mountChannelRoutesWithEngine 是否真的
// 用 publicGroup.BasePath() / authGroup.BasePath() 作为前缀——后者由
// TestMountChannelRoutesWithEngine_PrefixesFromGroupBasePath 经真实调用点守护。
func TestDeclaredRoutes_UsesRealGroupPrefixes(t *testing.T) {
	reg := &specRegistrar{
		public: []routeSpec{{http.MethodGet, "/p", statusHandler(http.StatusOK)}},
		auth:   []routeSpec{{http.MethodPost, "/a", statusHandler(http.StatusOK)}},
	}

	got, err := routeregistry.DeclaredRoutes(reg, "/api", "/api/channel")
	if err != nil {
		t.Fatalf("DeclaredRoutes returned error: %v", err)
	}

	set := make(map[string]bool, len(got))
	for _, k := range got {
		set[k.Method+" "+k.Path] = true
	}
	for _, want := range []string{"GET /api/p", "POST /api/channel/a"} {
		if !set[want] {
			t.Errorf("declared route %q missing; got %v (prefixes must come from the real groups)", want, got)
		}
	}
}

// TestMountChannelRoutesWithEngine_PrefixesFromGroupBasePath 经真实调用点锁定「试探期前缀取自
// 分组 BasePath()」——即 channel_routes.go 里 DeclaredRoutes(reg, publicGroup.BasePath(),
// authGroup.BasePath()) 的这两个实参，而不是硬编码常量。
//
// 关键手法：传入**非标准前缀**的公开组 /x 与鉴权组 /x/y，使「取 BasePath()」（读回 /x/...、
// /x/y/...）与「硬编码回 /api、/api/channel」（读回 /api/...）读回的路径截然不同。
//
// 判别原理：注册方在公开组、鉴权组**各**声明一条不冲突的 /clean，另按 conflictOnAuth 在
// 对应一侧追加一条与内置路由冲突的路径（同一组内 /clean 声明在前、冲突路由在后）。
//   - 前缀正确（取自 BasePath()）：DeclaredRoutes 读回的 /x/... 或 /x/y/... 能撞上同前缀内置路由
//     → 整方跳过 → 两条 /clean 都没挂 → 请求 /x/clean 与 /x/y/clean 均得 404；
//   - 前缀被硬编码回 /api、/api/channel：读回 /api/...，与 /x/... 的内置路由对不上 → 检测不到冲突
//     → 注册方被真实挂载 → 无论 safeMountRegistrar 先挂公开组还是鉴权组，都至少有一条 /clean
//     先挂成功（随后冲突路由触发 gin 重复注册 panic，被 safeMountRegistrar 的 recover 兜住）
//     → 该条 /clean 得非 404。
//
// 故断言「/x/clean 与 /x/y/clean 均返回 404」即可区分二者，从而锁住调用点前缀来自分组
// BasePath()；「两组各一条 clean」的设计使该判别**不依赖** safeMountRegistrar 的挂载顺序——
// 若只声明公开组一条 /clean，一旦将来改成先挂鉴权组，冲突会在鉴权组先 panic、公开组那条
// /clean 来不及挂载而返回 404，判别会假绿。公开、鉴权两个实参各设一个子用例：只有对应实参被
// 硬编码时该子用例才变红，两个实参都能被独立守护。
func TestMountChannelRoutesWithEngine_PrefixesFromGroupBasePath(t *testing.T) {
	tests := []struct {
		name           string
		builtinPath    string // 在真实前缀下预置的内置路由（冲突目标）
		conflictOnAuth bool   // 冲突路由声明在鉴权组（true）还是公开组（false）
	}{
		{"public prefix from BasePath", "/x/builtin/conflict", false},
		{"auth prefix from BasePath", "/x/y/builtin/conflict", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestEngine()
			// 非标准前缀下的一条内置路由：注册方的冲突声明与之撞 method+path。
			r.GET(tt.builtinPath, statusHandler(http.StatusTeapot))

			publicGroup := r.Group("/x")
			authGroup := r.Group("/x/y")

			// 公开组、鉴权组各声明一条不冲突的 /clean；同组内 /clean 声明在前、冲突路由在后
			// （顺序不可调换：先挂成功的 /clean 才是「漏检时被真实挂载」的可观测证据）。
			reg := &specRegistrar{
				public: []routeSpec{{http.MethodGet, "/clean", statusHandler(http.StatusOK)}},
				auth:   []routeSpec{{http.MethodGet, "/clean", statusHandler(http.StatusOK)}},
			}
			conflict := routeSpec{http.MethodGet, "/builtin/conflict", statusHandler(http.StatusOK)}
			if tt.conflictOnAuth {
				reg.auth = append(reg.auth, conflict)
			} else {
				reg.public = append(reg.public, conflict)
			}

			mountChannelRoutesWithEngine(r, publicGroup, authGroup, []routeregistry.RouteRegistrar{reg})

			// 检测生效 → 整方跳过 → 两条 /clean 都不存在 → 都 404。
			// 前缀被硬编码 → 漏检 → 真实挂载 → 无论先挂哪一组，都至少有一条 /clean 先挂成功
			// （随后冲突路由 panic 被 recover 兜住）→ 该条非 404。
			for _, path := range []string{"/x/clean", "/x/y/clean"} {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != http.StatusNotFound {
					t.Errorf("GET %s code=%d, want %d: mount must read the probe prefixes from the real groups' BasePath(), so a registrar declaring a conflicting route under those prefixes is skipped as a whole", path, w.Code, http.StatusNotFound)
				}
			}
		})
	}
}
