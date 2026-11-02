# file-s3-backup

通过官方 **AWS CLI v2** 将本地目录定时备份到 XSKY、AWS S3 或其他兼容 S3 的对象存储。Go 仅负责环境变量配置、定时调度、源目录检查和进程超时；文件传输直接调用 `aws s3 sync`。

沿用 [mysql-s3-backup](https://github.com/sxxpqp/mysql-s3-backup) 的部署方式，支持 Docker Compose 和 Kubernetes Deployment，调度默认使用北京时间。

**同步方向始终是本地 → S3，不会删除本地文件。** 源目录只读挂载。默认先预览，正式运行前需要修改配置。

程序在启动 AWS CLI 前硬校验命令：只允许本地目录上传到独立 S3 前缀，以及查询桶版本状态；拒绝反向同步、移动、删除命令和额外 CLI 参数。没有允许删除源文件的配置项。Compose 和 Kubernetes 模板同时以只读方式挂载源目录；部署时必须保留该限制，提供操作系统层面的写入、删除保护。

## 工作方式

```text
主机 1 /www/wwwroot ──只读──→ s3://wishfoxs-web-backup/web-01/www/wwwroot/
主机 2 /www/wwwroot ──只读──→ s3://wishfoxs-web-backup/web-02/www/wwwroot/
```

- 每台主机部署一个实例，使用独立 `S3_PREFIX`；不要让多个实例同时同步同一前缀。
- AWS CLI 负责差异判断、分段上传、重试和传输。默认主要比较文件大小和修改时间，不逐文件比较内容哈希。
- 同步前检查源目录中的 `.s3-backup-source` 标记，拒绝空目录；跳过软链接并记录日志，不跟随链接读取目标；特殊文件或目录遍历失败会停止本轮。链接目标的真实目录若位于备份源内，会按真实路径正常备份；位于源目录之外的目标内容不会备份。
- 预览对应 `--dryrun`；删除同步对应 `--delete`，只影响 S3 中该前缀下的当前对象。
- 同一实例串行执行；本轮失败记录日志并在下一个计划时间重试。单次执行失败返回非零退出码。

工具不会创建桶、开启多版本或创建 XSKY 快照，这些需要在对象存储平台配置。

## Docker Compose 快速开始

需要 Docker 和 Docker Compose v2。使用兼容写法 `env_file: [.env]`，不依赖 Compose 2.30 才引入的 `format: raw`。镜像提供 linux/amd64；宿主机无需安装 Go、Python 或 AWS CLI。

```bash
git clone https://github.com/sxxpqp/file-s3-backup.git
cd file-s3-backup
cp .env.example .env
chmod 600 .env
vi .env
```

至少填写以下参数。S3 接口地址不是管理后台地址，也不要带桶名或路径：

```dotenv
S3_ENDPOINT=http://YOUR-XSKY-S3-ENDPOINT
S3_BUCKET=wishfoxs-web-backup
S3_PREFIX=web-01/www/wwwroot
AWS_REGION=us-east-1
AWS_ACCESS_KEY_ID=replace-me
AWS_SECRET_ACCESS_KEY=replace-me
BACKUP_INTERVAL=0 2 * * *
BACKUP_TIMEZONE=Asia/Shanghai
BACKUP_DELETE=false
BACKUP_DRY_RUN=true
```

第二台主机使用 `S3_PREFIX=web-02/www/wwwroot`。区域按服务商要求填写。

支持 HTTP 接口，例如 `S3_ENDPOINT=http://192.168.1.10:8080`，协议和端口会原样传给 AWS CLI；无需添加 `--no-verify-ssl`。修改 `.env` 后执行 `docker compose up -d --force-recreate` 生效。

`.env` 使用标准 Compose 解析规则。密钥包含 `$`、`#` 等特殊字符时，用单引号包住整个值，例如 `AWS_SECRET_ACCESS_KEY='example$literal#secret'`；单引号不会传入容器。不要将 `docker compose config` 的完整输出发到聊天或日志中，其中可能含密钥；仅检查配置请使用 `docker compose config --quiet`。

Compose 默认将宿主机 `/www/wwwroot` 只读挂载到容器 `/source`。若要更换源目录，修改 `compose.yaml` 的 bind source；`BACKUP_SOURCE` 填容器内路径。

### 先预览

确认实际数据卷已经挂载，在真实源目录创建标记。不要把创建标记放进自动启动脚本，否则可能掩盖挂载故障。

```bash
test -d /www/wwwroot && touch /www/wwwroot/.s3-backup-source
docker compose pull
docker compose run --rm -e BACKUP_INTERVAL= -e BACKUP_DRY_RUN=true backup
```

默认拉取 `registry.cn-hangzhou.aliyuncs.com/sxxpqp/file-s3-backup:latest`。私有仓库先执行 `docker login registry.cn-hangzhou.aliyuncs.com`。镜像需要先通过下文 CI 或手动构建发布。

容器运行身份为 UID 1000 / GID 1002，必须能遍历和读取源目录。按实际权限调整 Compose 的 `user`；不要为备份递归修改现网文件权限。SELinux enforcing 主机需配置适当的文件访问策略。

### 正式运行

预览确认目标路径和范围正确后，把 `.env` 的 `BACKUP_DRY_RUN` 改为 `false`：

```bash
# 先执行一次真实备份，成功后下载若干文件验证
docker compose run --rm -e BACKUP_INTERVAL= backup
# 开始定时运行
docker compose up -d
docker compose logs --tail 100 -f
```

正式运行前不要同时启动手动任务和后台任务。修改 `.env` 后重建容器使新配置生效：

```bash
docker compose up -d --force-recreate
```

更新镜像：

```bash
docker compose pull
docker compose up -d
```

可在 `.env` 中设置 `BACKUP_IMAGE=完整镜像地址:标签` 覆盖默认值。生产环境建议使用已发布的固定版本标签。

## 配置说明

| 参数 | 默认值 / 说明 |
| --- | --- |
| `BACKUP_SOURCE` | `/source`，容器内绝对路径 |
| `S3_ENDPOINT` | 必填，HTTP 或 HTTPS S3 接口地址，可带端口 |
| `S3_BUCKET` | 必填，预先创建的桶 |
| `S3_PREFIX` | 必填，每个实例独立前缀，不能指向桶根目录 |
| `AWS_REGION` | `us-east-1`，按服务商要求配置 |
| `AWS_ACCESS_KEY_ID` | 必填，专用备份账号 AK |
| `AWS_SECRET_ACCESS_KEY` | 必填，专用备份账号 SK |
| `AWS_SESSION_TOKEN` | 可选，STS 临时凭据 token |
| `BACKUP_INTERVAL` | 留空仅运行一次；示例为 `0 2 * * *` |
| `BACKUP_TIMEZONE` | `Asia/Shanghai`，调度和应用日志时区 |
| `BACKUP_TIMEOUT` | `2h`，每轮 CLI 执行共用时限 |
| `BACKUP_DELETE` | `false`，是否传递 `--delete` |
| `BACKUP_DRY_RUN` | 程序默认 `false`，部署示例为 `true`；预览不上传、不删除 |
| `AWS_CLI_BIN` | `aws`，本地运行时可指定 CLI 可执行文件 |
| `AWS_CONFIG_FILE` | 镜像内已指向 `/etc/file-s3-backup/aws-config` |

调度支持两种形式：

```dotenv
# 每天北京时间 02:00；启动后等待下一次计划时间
BACKUP_INTERVAL=0 2 * * *
# 每 30 分钟
BACKUP_INTERVAL=*/30 * * * *
# 也支持时长：启动立即执行，此后每轮结束等待 24 小时
BACKUP_INTERVAL=24h
# 单次执行
BACKUP_INTERVAL=
```

错过的 cron 时间不补跑。每轮执行可能持续到超时；Go 预检查中的文件系统调用不能被强制中断。

镜像的 `aws-config` 默认使用 path addressing 和 4 个传输并发。请求/响应 checksum 策略设为 `WHEN_REQUIRED`，以适配兼容服务，可通过 AWS CLI 环境变量覆盖。使用私有 CA 时挂载证书并设置 `AWS_CA_BUNDLE`，不关闭 TLS 校验。

## 多版本、删除同步和 XSKY 快照

| 配置 | 本地文件删除后 | 同名文件修改后 |
| --- | --- | --- |
| `BACKUP_DELETE=false` | S3 当前对象仍保留 | 上传检测到的新内容，旧内容依赖多版本保留 |
| `BACKUP_DELETE=true` | S3 当前对象也删除 | 同上 |

**两种模式都不会删除本地文件。** 开启删除同步前，程序通过 `aws s3api get-bucket-versioning` 检查桶状态必须为 `Enabled`，否则本轮停止。

建议先保持 `false` 完成首次备份和恢复验证。如果希望 S3 当前目录反映本地现状，可在确认多版本恢复有效、快照计划及保留周期正确后改为：

```dotenv
BACKUP_DELETE=true
BACKUP_DRY_RUN=false
```

XSKY 的“支持快照”开关不等于已经创建快照。本工具不调用 XSKY 专有快照接口，也不等待快照成功；如果必须保证“快照成功后才能同步删除”，先不要启用删除同步。

按标准 S3 多版本语义，不指定 version ID 的普通删除会创建删除标记，历史版本仍保留；请在购买的 XSKY 版本用测试文件验证这一行为。历史版本和快照占用空间，生命周期和快照清理规则决定可恢复期限。

AWS CLI 自行安排上传与删除顺序，不保证全部上传完成后才删除。源目录标记只能发现部分挂载问题，无法防止所有批量误删或运行中源目录变化。

## Kubernetes 部署

使用 `deploy/k8s/deployment.yaml`：单副本、Recreate 策略，应用内置 cron 调度。Secret、Deployment、源 PVC 必须在同一个 namespace。

1. 把 Deployment 的镜像改为实际已发布的标签；CI 的版本 tag 为 `v1.0.0` 等带 `v` 标签，模板中的 `1.0.0` 是手动构建示例。
2. 修改 PVC 名称、UID/GID；私有仓库按需配置 `imagePullSecrets`。
3. 通过现有业务容器，在真实 PVC 源目录创建 `.s3-backup-source` 标记，确认备份身份可读。
4. 配置并应用 Secret，然后启动定时任务。

```bash
cp deploy/k8s/secret.yaml.example deploy/k8s/secret.yaml
chmod 600 deploy/k8s/secret.yaml
vi deploy/k8s/secret.yaml
kubectl -n mall-prod apply -f deploy/k8s/secret.yaml
kubectl -n mall-prod apply -f deploy/k8s/deployment.yaml
kubectl -n mall-prod logs -f deployment/file-s3-backup
```

示例默认 `BACKUP_DRY_RUN=true`，可先把周期设为 `24h`，立即进行一次预览，然后等待。核对后将预览改为 `false` 并设置所需 cron：

```bash
kubectl -n mall-prod apply -f deploy/k8s/secret.yaml
kubectl -n mall-prod rollout restart deployment/file-s3-backup
```

不要在常驻 Deployment 中把周期留空：单次执行结束后控制器会不断重启容器；需要单次执行时使用 Job。

示例挂载整个 `sohu-shop-web-base`，备份整个网站目录。仅备份附件时可设置 `BACKUP_SOURCE=/source/data/attachment`，标记也应位于附件目录。

RWO 卷可能要求备份 Pod 和业务 Pod 同节点，应按现网配置节点亲和性；RWOP 无法同时挂给另一个 Pod。不要盲目修改访问模式或重启业务。普通主机 `/www/wwwroot` 不会自动出现在 PVC 内，主机备份使用 Compose 更直接。

## 备份账号权限

账号权限限制到本机前缀：普通同步需要 `s3:ListBucket`、`s3:PutObject`、`s3:AbortMultipartUpload`；开启删除同步还需要桶级 `s3:GetBucketVersioning` 和前缀内 `s3:DeleteObject`。额外加密策略可能要求其他权限。

不要给备份账号 `s3:DeleteObjectVersion`、修改版本控制或管理快照的权限。恢复使用单独的管理账号。凭据只放本地 `.env` 或 Kubernetes Secret，不提交到 Git。

## 恢复和验证

首次上线后，至少验证：首次上传、修改文件再次上传、删除源文件后恢复历史内容。使用测试目录进行，避免直接在业务文件上演练。

恢复到空目录，核对后再替换业务文件。通过安装了 AWS CLI 的恢复主机，使用独立的只读/恢复账号：

```bash
aws --endpoint-url http://YOUR-S3-ENDPOINT s3 sync \
  s3://wishfoxs-web-backup/web-01/www/wwwroot/ /tmp/www-restore/
```

上述命令只恢复当前可见对象。已删除或被覆盖的内容，需要在 XSKY 控制台选择历史版本、下载指定版本，或从删除前的快照提取。桶级回滚可能影响同桶两台主机，优先恢复对应前缀。

限制：只备份文件内容和路径，不保留权限、属主、空目录或软链接；不是数据库备份。同步过程中源文件变化可能得到混合时间点数据，需要一致性恢复点时配合源端快照或暂停写入。大小和时间都未改变的内容修改可能不被 sync 发现。

无内置告警，需把失败日志接入现有监控；容器处于运行状态不代表最近一次备份成功。Compose 日志已配置大小轮转。

## 构建、测试与发布

```bash
go test ./...
go vet ./...
docker compose config --no-env-resolution --quiet
# 如需本地构建或手动发布
docker build -t registry.cn-hangzhou.aliyuncs.com/sxxpqp/file-s3-backup:1.0.0 .
docker push registry.cn-hangzhou.aliyuncs.com/sxxpqp/file-s3-backup:1.0.0
```

最终镜像基于固定版本 `amazon/aws-cli:2.39.2`，增加 Go 调度程序和 AWS 配置。原生执行 Go 二进制需要自行安装 AWS CLI v2，并设置 `AWS_CONFIG_FILE` 指向本项目的 `aws-config`。

GitHub Actions 在 PR 执行 `go test -race` 和 `go vet`；main 推送和 `v*` tag 测试通过后构建 linux/amd64 镜像发布到 Aliyun ACR。

在仓库 Settings → Secrets and variables → Actions 中配置：

| Secret | 说明 |
| --- | --- |
| `ALIYUN_REGISTRY` | `registry.cn-hangzhou.aliyuncs.com`，不带协议 |
| `ALIYUN_NAME_SPACE` | 镜像命名空间，如 `sxxpqp` |
| `ALIYUN_REGISTRY_USER` | 仓库登录用户名 |
| `ALIYUN_REGISTRY_PASSWORD` | 仓库登录密码 |

main 发布 `latest` 和 commit SHA 标签；`v1.0.0` tag 发布同名 `v1.0.0` 镜像标签。推送代码不等于镜像已经发布成功，需查看 Actions 结果。

参考：[AWS CLI sync](https://docs.aws.amazon.com/cli/latest/reference/s3/sync.html)、[官方 CLI 镜像](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-docker.html)。
