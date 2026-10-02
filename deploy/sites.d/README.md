# 额外站点

公网叠加部署（`docker-compose.public.yml`）时，Web 容器的 Caddy 会导入这个目录里的每个 `*.caddy` 文件，让其他站点和 SinthMux 共用 80/443 并自动申请证书。目录为空时不影响主站。

可在 `deploy/.env.local` 中用 `SINTHMUX_EXTRA_SITES_DIR` 指向其他目录。被代理的服务需接入同一 Docker 网络（默认 `deploy_default`），例如：

```caddy
example.com {
	reverse_proxy example-site:8080
}
```

修改后重建 Web 容器生效：`docker compose --env-file deploy/.env.local -f deploy/docker-compose.yml -f deploy/docker-compose.public.yml up -d --no-build web`。
