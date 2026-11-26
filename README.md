# MySQL 网关

客户端通过同一端口选择数据库别名。示例中的 `A` 经 SSH 跳板机 `192.168.1.10:22` 访问 `mysql.internal:3306` 上的 `AA` 库。目标地址由跳板机解析和连接。

## 启动和连接

1. 复制 `config.example.yaml` 为 `config.yaml`，填写目标地址、真实库名和账号。
2. 在 `gateway.password` 填写客户端登录网关的密码，在 `proxy.ssh_jump.password` 填写跳板机账号的密码，在 `mappings.A.password` 填写目标 MySQL 账号的密码。每个账号都可单独选择 `password` 或 `password_env`，同时填写两项会在启动时报错。
3. 在 `known_hosts` 指定的文件中放入已核实的跳板机主机公钥。使用私钥登录时，将跳板机的密码字段替换为 `private_key: ssh/id_ed25519`。相对路径以配置文件目录为基准。
4. 启动网关，再在另一个终端连接。

```sh
go run . -config config.yaml -lip 127.0.0.1 -lport 3306
```

```sh
mysql -h 127.0.0.1 -P 3306 -u gateway -p -D A
```

`SELECT DATABASE()` 返回 `AA`。不带 `-D A` 登录时，先执行 `USE A`。每个客户端连接独占一个后端会话，切换到另一条映射需要新建连接。显式限定表名时使用真实库名，例如 `AA.orders`。目标 MySQL 账号的授权决定可访问的表和库。

在 `mappings` 中添加条目即可增加别名。删除某条映射的 `proxy` 字段即可让网关直接连接该目标。配置在启动时读取。网关默认监听本机回环地址，客户端连接使用 MySQL 原生密码认证。`proxy` 可以引用 `type: ssh` 的跳板机，也可以引用 `type: socks5` 的 SOCKS5 代理。SOCKS5 代理可以省略认证字段，也可以使用 `user` 与 `password` 或 `password_env`。

映射可以设置 `read_only: true`。网关会在转发普通查询和预处理语句前拦截写入、DDL、权限管理等语句，并允许查询、元数据、会话设置和事务控制语句。解析失败或无法识别的语句会被拦截。

同一个 SSH 代理配置名下的映射共享一个 SSH Client，每个客户端后端会话使用独立的 SSH TCP channel，因此目标地址、目标数据库、登录账号、事务和预处理语句仍各自隔离。所有使用该 SSH 配置的后端会话关闭后，网关默认等待 5 分钟再关闭 SSH Client。可通过顶层 `ssh_idle_timeout` 使用 Go duration 格式调整等待时间，例如 `30s` 或 `10m`。配置必须是正值。SOCKS5 连接按后端会话单独建立。

## 验证

`go test ./...` 运行协议和 SSH 路由测试。已安装 MariaDB 的 Linux 环境可以运行以下测试，使用临时数据目录和随机端口验证事务、预处理语句和会话隔离。

```sh
go test -count=1 ./... -args -mariadb-basedir /usr
```

也可设置 `SQLTR_TEST_MYSQL_ADDR`、`SQLTR_TEST_MYSQL_DB`、`SQLTR_TEST_MYSQL_USER` 和 `SQLTR_TEST_MYSQL_PASSWORD` 后运行 `go test -count=1 ./...`。测试账号需要在测试库创建临时表的权限。
