package router

import (
	"github.com/pai801/myapi/controller"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/relay/routeregistry"

	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

func SetApiRouter(router *gin.Engine) {
	apiRouter := router.Group("/api")
	apiRouter.Use(gzip.Gzip(gzip.DefaultCompression))
	apiRouter.Use(middleware.GlobalAPIRateLimit())
	{
		apiRouter.GET("/status", controller.GetStatus)
		apiRouter.GET("/models", middleware.UserAuth(), controller.DashboardListModels)

		userRoute := apiRouter.Group("/user")
		{
			userRoute.POST("/login", middleware.CriticalRateLimit(), controller.Login)
			userRoute.GET("/logout", controller.Logout)

			selfRoute := userRoute.Group("/")
			selfRoute.Use(middleware.UserAuth())
			{
				selfRoute.GET("/dashboard", controller.GetUserDashboard)
				selfRoute.GET("/self", controller.GetSelf)
				selfRoute.PUT("/self", controller.UpdateSelf)
				selfRoute.GET("/token", controller.GenerateAccessToken)
				selfRoute.GET("/available_models", controller.GetUserAvailableModels)
			}

			adminRoute := userRoute.Group("/")
			adminRoute.Use(middleware.AdminAuth())
			{
				adminRoute.GET("/", controller.GetAllUsers)
				adminRoute.GET("/search", controller.SearchUsers)
				adminRoute.GET("/:id", controller.GetUser)
				adminRoute.POST("/", controller.CreateUser)
				adminRoute.POST("/manage", controller.ManageUser)
				adminRoute.PUT("/", controller.UpdateUser)
				adminRoute.DELETE("/:id", controller.DeleteUser)
			}
		}
		optionRoute := apiRouter.Group("/option")
		optionRoute.Use(middleware.RootAuth())
		{
			optionRoute.GET("/", controller.GetOptions)
			optionRoute.PUT("/", controller.UpdateOption)
		}
		channelRoute := apiRouter.Group("/channel")
		channelRoute.Use(middleware.AdminAuth())
		{
			channelRoute.GET("/", controller.GetAllChannels)
			channelRoute.GET("/search", controller.SearchChannels)
			channelRoute.GET("/models", controller.ListAllModels)
			// 渠道能力清单（PRD §5.12 层1 / D7）：前端据此渲染渠道类型与通用面板，
			// 本仓不注册任何渠道，故返回空列表；扩展方注册的渠道由其在 init() 期注入。
			channelRoute.GET("/descriptors", controller.GetChannelDescriptors)
			channelRoute.GET("/:id", controller.GetChannel)
			channelRoute.GET("/reset/:id", controller.ResetChannel)
			channelRoute.GET("/test", controller.TestChannels)
			channelRoute.GET("/test/:id", controller.TestChannel)
			channelRoute.GET("/update_balance", controller.UpdateAllChannelsBalance)
			channelRoute.GET("/update_balance/:id", controller.UpdateChannelBalance)
			channelRoute.POST("/fetch_models", controller.FetchChannelModels)
			channelRoute.POST("/", controller.AddChannel)
			channelRoute.PUT("/", controller.UpdateChannel)
			channelRoute.DELETE("/disabled", controller.DeleteDisabledChannel)
			channelRoute.DELETE("/:id", controller.DeleteChannel)
		}
		tokenRoute := apiRouter.Group("/token")
		tokenRoute.Use(middleware.UserAuth())
		{
			tokenRoute.GET("/", controller.GetAllTokens)
			tokenRoute.GET("/search", controller.SearchTokens)
			tokenRoute.GET("/:id", controller.GetToken)
			tokenRoute.POST("/", controller.AddToken)
			tokenRoute.PUT("/", controller.UpdateToken)
			tokenRoute.DELETE("/:id", controller.DeleteToken)
		}
		logRoute := apiRouter.Group("/log")
		logRoute.GET("/", middleware.AdminAuth(), controller.GetAllLogs)
		logRoute.GET("/stat", middleware.AdminAuth(), controller.GetLogsStat)
		logRoute.GET("/self/stat", middleware.UserAuth(), controller.GetLogsSelfStat)
		logRoute.GET("/search", middleware.AdminAuth(), controller.SearchAllLogs)
		logRoute.GET("/self", middleware.UserAuth(), controller.GetUserLogs)
		logRoute.GET("/self/search", middleware.UserAuth(), controller.SearchUserLogs)
		logRoute.GET("/active", middleware.AdminAuth(), controller.GetAllActiveLogs)
		logRoute.GET("/:id", middleware.AdminAuth(), controller.GetLogDetail)
		// SSE 端点：Web 路由中的全局 gzip 已跳过 /api/ 路径，不会缓冲
		// OPTIONS preflight 不经过 AdminAuth（跨域请求不带 cookie）
		router.OPTIONS("/api/log/active/events", controller.HandleSSEOptions)
		router.GET("/api/log/active/events", middleware.AdminAuth(), controller.StreamActiveLogs)
		groupRoute := apiRouter.Group("/group")
		groupRoute.Use(middleware.AdminAuth())
		{
			groupRoute.GET("/", controller.GetGroups)
			groupRoute.GET("/list", controller.GetGroupList)
			groupRoute.GET("/:id", controller.GetGroup)
			groupRoute.POST("/", controller.AddGroup)
			groupRoute.PUT("/:id", controller.UpdateGroup)
			groupRoute.DELETE("/:id", controller.DeleteGroup)
		}
		modelMetadataRoute := apiRouter.Group("/model-metadata")
		modelMetadataRoute.Use(middleware.AdminAuth())
		{
			modelMetadataRoute.GET("/", controller.GetAllMetadata)
			modelMetadataRoute.GET("/:name", controller.GetMetadata)
			modelMetadataRoute.POST("/", controller.CreateMetadata)
			modelMetadataRoute.PUT("/", controller.UpdateMetadata)
			modelMetadataRoute.DELETE("/:name", controller.DeleteMetadata)
		}
		// 渠道插件化（PRD §5.11）：渠道专属的登录与回调路由改由路由注册表在启动期装配。
		// 注册方在 init() 期只登记自己（见 router/channel_routes.go）；此处才真正挂到公开组
		// （apiRouter，仅 gzip + 全局限流）与鉴权组（channelRoute，含 AdminAuth）。
		// 中间件由分组决定，注册方碰不到；本仓不注册任何渠道 → 注册表为空 → 这些路由不存在 → 404。
		//
		// 刻意放在全部内置路由注册完之后：mountChannelRoutesWithEngine 用 engine.Routes()
		// 取「内置基线」做冲突检测，基线必须含**全部**内置路由，否则注册方若撞上此处之后才
		// 注册的内置路由将检测不到，panic 会落在 safeMountRegistrar 的 recover 覆盖之外，
		// 带崩整个 SetApiRouter。放到末尾后，冲突 panic 也发生在 safeMountRegistrar 内部。
		// 移动不改变匹配结果：注册方声明的路由只与 /api/channel/* 内置路由同子树，
		// 而这些内置路由本就注册在挂载点之前，相对顺序不变。
		mountChannelRoutesWithEngine(router, apiRouter, channelRoute, routeregistry.Registered())
	}
}
