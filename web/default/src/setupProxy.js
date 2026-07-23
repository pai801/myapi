const { createProxyMiddleware } = require('http-proxy-middleware');

// CRA dev server 默认启用 gzip 压缩，会缓冲 SSE 响应。
// 仅对 SSE 路由清除 content-encoding，其他 /api 响应原样透传。
module.exports = function (app) {
  app.use(
    '/api',
    createProxyMiddleware({
      target: 'http://localhost:3000',
      changeOrigin: true,
      on: {
        proxyRes: (proxyRes, req) => {
          if (req.url === '/api/log/active/events') {
            delete proxyRes.headers['content-encoding'];
          }
        },
      },
    })
  );
};
