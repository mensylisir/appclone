package i18n

// messages 是唯一的人类文案来源。写法约束（§6.1 文案红线）：
//   - 不说"命中 N 次"，只说"观测到 N 个写入条目"；
//   - 不说"结果完整"；
//   - 对 shared_writes 不说"已隔离"；
//   - 失败必须带"下一步做什么"，不许只写"失败"。
var messages = map[string]map[string]string{
	"zh_CN": {
		// 路径
		"err.path.empty":        "路径是空的，无法使用。",
		"err.path.not_absolute": "「{path}」不是绝对路径，appclone 只接受绝对路径或 ~/ 开头。",
		"err.path.dotdot":       "「{path}」里包含 ..，这类路径无法安全展开。",
		"err.path.unknown_var":  "「{path}」里的变量 appclone 不认识（只支持 $HOME 与 $XDG_*）。",

		// 环境
		"err.env.no_home":       "找不到当前用户的家目录，无法确定应用数据位置。",
		"err.env.base_relative": "数据目录（AC_BASE）必须是绝对路径。",
		"err.env.mkdir":         "无法创建目录 {path}，请检查磁盘空间与权限。",
		"err.env.write":         "无法写入 {path}，请检查磁盘空间与权限。",
		"err.env.chmod":         "无法设置 {path} 的权限，请检查文件系统是否只读。",
		"err.env.open_lock":     "无法打开锁文件 {path}。",
		"err.env.flock":         "无法锁定 {path}，文件系统可能不支持文件锁。",
		"err.env.read_dir":      "无法读取 {path}。",
		"err.env.stat":          "无法检查 {path} 的状态。",
		"err.env.no_gio":        "系统缺少 gio（glib2 提供），无法把数据移入回收站。请安装 glib2 后重试；appclone 不会用 rm 代替它。",
		"err.env.trash_failed":  "移入回收站失败：{output}",

		"err.env.root":           "appclone 不能用 root 运行：root 会话下整个根文件系统是可写的，误操作会改到宿主系统；而且 root 下 bwrap 本来就能跑通，验证结果会掩盖普通用户的问题。请用你自己的账号运行。",
		"err.env.no_dbus_daemon": "系统里没有 dbus-daemon，无法为这个分身启动私有会话总线；分身将与主实例共用会话总线，可能无法双开。",

		// 运行
		"err.run.start_failed": "启动失败（{exec}）。请用 appclone doctor 检查 bwrap 与命名空间是否可用。",

		// 锁与并发
		"err.lock.timeout": "另一个 appclone 正在操作，请等它结束后重试（已等待 {seconds} 秒）。",

		// 应用
		"err.app.invalid_key":           "「{app}」不是合法的应用标识，只允许小写字母、数字、点、下划线、短横线。",
		"err.app.reserved_prefix":       "「{app}」使用了 appclone 的保留前缀，请换一个名字。",
		"err.app.not_found":             "找不到应用「{app}」，它可能已被卸载。",
		"err.app.no_exec":               "这个应用没有可用的启动命令（Exec=），无法为它创建分身。",
		"err.app.exec_unresolved":       "启动命令「{exec}」解析失败（来自 {path}），appclone 需要一个绝对路径才能启动它。",
		"err.app.exec_unbalanced_quote": "启动命令「{exec}」里的引号没有配对，无法解析。",

		// 档案
		"err.profile.not_found":           "应用「{app}」还没有隔离档案，需要先做一次探测。",
		"err.profile.parse":               "档案 {path} 无法解析，可能已损坏。请重新探测生成。",
		"err.profile.version_ahead":       "「{app}」的档案由更新版本的 appclone 生成（版本 {version}，当前支持 {current}）。请升级 appclone，不要用旧版改写它。",
		"err.profile.empty":               "「{app}」没有可隔离的应用数据，无法创建分身。",
		"err.profile.no_exec":             "档案里没有启动命令（exec），无法运行。请重新探测。",
		"err.profile.exec_relative":       "档案里的启动命令「{exec}」不是绝对路径，appclone 拒绝加载（相对路径会随工作目录变化指向别的东西）。",
		"err.profile.exec_missing":        "启动命令「{exec}」不存在，「{app}」可能已升级或卸载，请重新探测。",
		"err.profile.stale":               "「{app}」的档案已失效：{path} 已不存在。请重新配置（它会先重新探测再创建分身）。",
		"err.profile.root_path":           "隔离路径不能是根目录（字段 {field}）。",
		"err.profile.home_path":           "隔离路径不能是整个家目录（字段 {field}），那会把所有应用数据一起搬走。",
		"err.profile.public_path":         "「{path}」是公共目录，必须始终共享（字段 {field}）。分身就该和主实例共用下载、文档这些位置。",
		"err.profile.under_public_path":   "「{path}」在公共目录「{public}」之下，隔离它会让两个分身各自保存一份（字段 {field}）。",
		"err.profile.public_ancestor":     "「{path}」是公共目录「{public}」的上级，隔离它会连带隔离公共目录（字段 {field}）。",
		"err.profile.base_path":           "「{path}」位于 appclone 自己的数据目录内，会造成绑定自指（字段 {field}）。",
		"err.profile.protected_path":      "「{path}」是系统级配置目录，不允许整体隔离（字段 {field}）。请选择具体的应用子目录。",
		"err.profile.blanket_path":        "「{path}」不能写进 {field}：它等于放行整个主目录，隔离校验会因此对任何越界写入都说通过。请写具体的目录。",
		"err.profile.isolate_and_share":   "「{path}」既在 paths 里又在 shared_writes 里（后者是 {other}）：同一路径不能既隔离又共享，两者谁生效取决于 bind 顺序。请只保留一个。",
		"err.profile.reserved_env":        "档案里的环境变量 {key} 由 appclone 自己产出，不允许覆盖。",
		"err.profile.lock_is_socket":      "「{path}」同时被声明为锁文件和 socket。socket 不能用空文件顶替（应用会报 ENOTSOCK 起不来），请只保留 sockets 里的那一条。",
		"err.profile.extra_bind_format":   "额外绑定「{value}」格式不对，应为「源|目标」。",
		"err.profile.extra_bind_relative": "额外绑定的源「{src}」必须是绝对路径。",
		"err.profile.extra_bind_overlap":  "额外绑定的目标「{path}」与隔离路径重复，档案自相矛盾。",

		// 实例
		"err.instance.invalid_id":   "「{id}」不是合法的分身标识，只允许小写字母、数字、下划线、短横线，且不能以符号开头。",
		"err.instance.exists":       "「{app}」下已经有「{id}」这个分身了。已有的：{existing}",
		"err.instance.not_found":    "「{app}」下没有「{id}」这个分身。",
		"err.instance.parse":        "分身元数据 {path} 无法解析。",
		"err.instance.bad_time":     "分身元数据 {path} 里的时间格式不对（应为 RFC3339 UTC）。",
		"err.instance.running":      "「{app}」的这个分身正在运行，请先在应用里退出它。",
		"err.instance.id_exhausted": "「{app}」下的分身数量已达上限，无法再自动分配名字。",

		// 删除
		"err.trash.outside": "拒绝删除 {path}：它不在 appclone 自己的数据目录里。为了不误删你的文件，appclone 只会把 instances/、profiles.d/ 与 appclone-*.desktop 移入回收站。",

		// 显示名
		"err.display_name.empty":        "名字不能为空。",
		"err.display_name.too_long":     "名字最长 {max} 个字，「{name}」太长了。",
		"err.display_name.control_char": "名字里不能有换行或控制字符（那会破坏桌面入口）。",
		"err.display_name.duplicate":    "已经有同名分身「{name}」了，换一个名字。",

		// 桌面入口
		"err.launcher.no_cli": "找不到 appclone 命令行程序，无法生成可点击的桌面入口。请重新安装 appclone。",

		// 探测（discover.md 的三层管线）
		// 探测（discover.md 三层管线）
		"err.probe.no_command":         "没有要运行的应用命令，无法开始探测。",
		"err.probe.app_running":        "「{exec}」还有进程在运行。请先把它完全退出再开始跟踪——开着它探测只会得到零写入，看起来像「这个应用不写盘」。",
		"err.probe.no_fuse_overlayfs":  "缺少 fuse-overlayfs，无法用 overlay 方式探测。安装后重试；strace 降级路径本版本尚未实现。",
		"err.probe.no_fusermount":      "缺少 fusermount3/fusermount，无法卸载探测用的 overlay 挂载点。",
		"err.probe.no_dev_fuse":        "打不开 /dev/fuse（设备不存在或被沙箱挡住），无法挂载 overlay。",
		"err.probe.mount_failed":       "挂载探测写层失败：{output}",
		"err.probe.umount_failed":      "卸载探测写层失败：{output}。挂载点可能还挂着，下次探测会报 EBUSY。",
		"err.probe.session":            "探测会话异常结束：{message}",
		"err.probe.start_failed":       "无法启动探测会话（{exec}）。",
		"err.probe.setsid_failed":      "无法为探测会话建立独立会话（setsid 失败）；为避免误杀你的终端，只结束了单个进程。",
		"err.probe.no_upper":           "探测写层 {path} 不存在或不可读，无法归因写入。",
		"err.probe.workdir_in_home":    "探测写层 {path} 落在你的家目录（{home}）里：overlay 的 upper 不能嵌在 lower 内，否则视图会递归。",
		"err.probe.contaminated":       "本次探测作废：公共目录里出现了写入（{paths}），说明「把公共目录绑回真实位置」没有生效，你的文件可能已经落进隔离层。现场已保留在 {quarantine}，请检查后再决定是否删除。",
		"err.probe.bind_verify_failed": "探测会话里发现 {path} 仍然落在隔离层（而不是被绑回真实目录）。这说明公共目录的 bind 没有生效，本次探测作废——此时应用还没启动，所以没有文件被写进去。",
		"err.probe.no_user_dirs":       "读不到 {path}（XDG 公共目录清单）。为了不误删你的文件，探测必须知道哪些目录是公共的；请先用桌面环境的设置工具配置一次（或确认 XDG_CONFIG_HOME 正确）。",
		"err.probe.bad_public_dir":     "公共目录 {path} 不在家目录（{home}）之下：这样的路径一旦被 bind 会把整个隔离视图遮掉，探测会变成在你的真实家目录里跑。请修正 user-dirs.dirs。",
		"err.probe.quarantine_failed":  "无法把探测现场改名保留到 {path} 同级目录。为避免删掉可能属于你的文件，appclone 停在这里没有删除任何东西——请手动检查该目录。",
		"err.probe.session_failed":     "探测会话没能跑起来（退出码 {code}）。常见原因：bwrap 无法创建 user namespace、或应用的可执行文件不存在。",
		"err.probe.timeout":            "探测超时（你一直没有关掉应用）。这次结果不完整，已作废并保留现场。",
		"err.probe.stale_mount":        "上一轮探测的挂载点 {path} 还在，卸载失败；清掉它之前不能开始新的探测。",
		"err.probe.unavailable":        "「{app}」还没有可用的隔离档案，而本版本尚未接入探测管线。请等待探测功能，或使用带内置模板的应用。",
		"err.probe.no_writes":          "没有跟踪到「{app}」的任何写入。请确认应用确实启动了（它可能已经在运行、或没有图形环境），然后重试。",
		"err.audit.unavailable":        "运行期审计层（--audit）还没有实现，本版本无法开启。不加这个参数可以直接运行；关闭审计不影响隔离本身。",
		"ui.add.reconfigure_note":      "「{app}」的档案已失效，正在用内置模板重建；若应用的数据目录已变化，请用「重新探测」校正。",

		// 语义分类（屏3 每条候选的说明文案，discover.md §5）
		"probe.why.config":  "应用配置或数据目录 — 建议隔离",
		"probe.why.data":    "数据库/索引文件 — 建议隔离",
		"probe.why.cache":   "缓存 — 默认不隔离（首次启动略慢，但省空间）",
		"probe.why.public":  "公共目录 — 强制共享，不能隔离",
		"probe.why.shared":  "共享可写 — 隔离后功能会坏（输入法、书签）",
		"probe.why.keyring": "凭证在密钥服务里，不在文件系统上 — 隔离不了",
		"probe.why.dotdir":  "写死在主目录顶层的目录 — 需要你确认里面没有你自己的文件",
		"probe.why.system":  "系统级路径 — 不建议隔离（会改到宿主）",
		"probe.why.unknown": "规则表没覆盖 — 需要你确认",

		// 桌面入口
		"desktop.clone_name_suffix": "（分身 {name}）",

		// 内部
		"err.internal.read_profile":    "读取档案 {path} 失败。",
		"err.internal.read_instance":   "读取分身元数据 {path} 失败。",
		"err.internal.encode_profile":  "序列化档案失败（内部错误）。",
		"err.internal.encode_instance": "序列化分身元数据失败（内部错误）。",
		"err.internal.no_resolver":     "内部错误：缺少路径解析器。",
		"err.internal.unknown":         "内部错误：{message}",
		"err.internal.usage":           "用法错误：{message}",
		"err.internal.bind_escape":     "内部错误：绑定路径逃出了实例目录（{root} / {path}）。",
	},

	"en": {
		"err.path.empty":        "Path is empty.",
		"err.path.not_absolute": "\"{path}\" is not absolute; appclone only accepts absolute paths or ~/.",
		"err.path.dotdot":       "\"{path}\" contains .., which cannot be expanded safely.",
		"err.path.unknown_var":  "Unknown variable in \"{path}\" (only $HOME and $XDG_* are supported).",

		"err.env.no_home":       "Cannot determine your home directory.",
		"err.env.base_relative": "The data directory (AC_BASE) must be an absolute path.",
		"err.env.mkdir":         "Cannot create directory {path}. Check disk space and permissions.",
		"err.env.write":         "Cannot write {path}. Check disk space and permissions.",
		"err.env.chmod":         "Cannot set permissions on {path}. Is the filesystem read-only?",
		"err.env.open_lock":     "Cannot open lock file {path}.",
		"err.env.flock":         "Cannot lock {path}; the filesystem may not support file locks.",
		"err.env.read_dir":      "Cannot read {path}.",
		"err.env.stat":          "Cannot stat {path}.",
		"err.env.no_gio":        "gio (from glib2) is missing, so data cannot be moved to the trash. Install glib2 and retry; appclone will not fall back to rm.",
		"err.env.trash_failed":  "Moving to trash failed: {output}",

		"err.env.root":           "appclone refuses to run as root: with root the whole filesystem is writable and a mistake can damage the host; also bwrap always works as root, which hides non-privileged failures. Run it as your own user.",
		"err.env.no_dbus_daemon": "dbus-daemon is missing, so no private session bus can be started; this clone will share the session bus and may fail to run alongside the original.",
		"err.run.start_failed":   "Starting failed ({exec}). Run appclone doctor to check bwrap and user namespaces.",

		"err.lock.timeout": "Another appclone is running. Retry when it finishes (waited {seconds}s).",

		"err.app.invalid_key":           "\"{app}\" is not a valid app id; only lowercase letters, digits, dot, underscore and dash are allowed.",
		"err.app.reserved_prefix":       "\"{app}\" uses appclone's reserved prefix.",
		"err.app.not_found":             "App \"{app}\" not found; it may have been uninstalled.",
		"err.app.no_exec":               "This app has no usable Exec= line, so a clone cannot be created.",
		"err.app.exec_unresolved":       "Cannot resolve the command \"{exec}\" (from {path}); an absolute path is required.",
		"err.app.exec_unbalanced_quote": "Unbalanced quotes in \"{exec}\".",

		"err.profile.not_found":           "No isolation profile for \"{app}\" yet; a probe run is needed first.",
		"err.profile.parse":               "Profile {path} cannot be parsed; it may be corrupted. Re-run the probe.",
		"err.profile.version_ahead":       "The profile for \"{app}\" was written by a newer appclone (version {version}, supported {current}). Upgrade appclone instead of rewriting it.",
		"err.profile.empty":               "\"{app}\" has no isolatable app data, so no clone can be created.",
		"err.profile.no_exec":             "The profile has no exec entry. Re-run the probe.",
		"err.profile.exec_relative":       "Profile exec \"{exec}\" is not absolute; appclone refuses to load it.",
		"err.profile.exec_field_code":     "Profile exec \"{exec}\" still contains a field code such as %U; field codes belong to desktop entries only.",
		"err.profile.extra_bind_missing":  "Extra bind source \"{path}\" does not exist; this bind would make the clone fail to start.",
		"err.profile.exec_missing":        "\"{exec}\" does not exist; \"{app}\" may have been upgraded or removed. Re-run the probe.",
		"err.profile.stale":               "The profile for \"{app}\" is stale: {path} no longer exists. Reconfigure it (it will probe again, then create the clone).",
		"err.profile.root_path":           "An isolation path cannot be the filesystem root (field {field}).",
		"err.profile.home_path":           "An isolation path cannot be your whole home directory (field {field}).",
		"err.profile.public_path":         "\"{path}\" is a public folder and must stay shared (field {field}).",
		"err.profile.under_public_path":   "\"{path}\" is inside the public folder \"{public}\"; isolating it would split your files between clones (field {field}).",
		"err.profile.public_ancestor":     "\"{path}\" is a parent of the public folder \"{public}\" (field {field}).",
		"err.profile.base_path":           "\"{path}\" is inside appclone's own data directory (field {field}).",
		"err.profile.protected_path":      "\"{path}\" is a system-wide config directory and cannot be isolated as a whole (field {field}). Pick the app's own subdirectory.",
		"err.profile.blanket_path":        "\"{path}\" cannot be listed in {field}: it allows the whole home directory, so the isolation check would pass any out-of-scope write. List a concrete directory instead.",
		"err.profile.isolate_and_share":   "\"{path}\" is both isolated (paths) and shared (shared_writes: {other}); the same path cannot be both, since which one wins depends on bind order. Keep only one.",
		"err.profile.reserved_env":        "Environment variable {key} is produced by appclone and cannot be overridden.",
		"err.profile.lock_is_socket":      "\"{path}\" is declared both as a lock file and a socket. A socket cannot be replaced by an empty file (the app fails with ENOTSOCK); keep only the sockets entry.",
		"err.profile.extra_bind_format":   "Extra bind \"{value}\" must be \"source|target\".",
		"err.profile.extra_bind_relative": "Extra bind source \"{src}\" must be absolute.",
		"err.profile.extra_bind_overlap":  "Extra bind target \"{path}\" duplicates an isolation path.",

		"err.instance.invalid_id":   "\"{id}\" is not a valid clone id; use lowercase letters, digits, underscore and dash, starting with a letter or digit.",
		"err.instance.exists":       "\"{app}\" already has a clone named \"{id}\". Existing: {existing}",
		"err.instance.not_found":    "\"{app}\" has no clone named \"{id}\".",
		"err.instance.parse":        "Clone metadata {path} cannot be parsed.",
		"err.instance.bad_time":     "Bad timestamp in {path} (expected RFC3339 UTC).",
		"err.instance.running":      "This clone of \"{app}\" is running. Quit it in the app first.",
		"err.instance.id_exhausted": "\"{app}\" has reached the maximum number of clones.",

		"err.trash.outside": "Refusing to delete {path}: it is not inside appclone's own data directory. To avoid deleting your files by mistake, appclone only moves instances/, profiles.d/ and appclone-*.desktop to the trash.",

		"err.display_name.empty":        "The name cannot be empty.",
		"err.display_name.too_long":     "The name must be at most {max} characters; \"{name}\" is too long.",
		"err.display_name.control_char": "The name cannot contain newlines or control characters (they break the desktop entry).",
		"err.display_name.duplicate":    "A clone named \"{name}\" already exists.",

		"err.launcher.no_cli": "Cannot locate the appclone command, so the desktop entry cannot be created. Reinstall appclone.",

		"err.probe.no_command":         "There is no command to run, so the probe cannot start.",
		"err.probe.app_running":        "\"{exec}\" is still running. Quit it completely first: probing while it runs yields zero writes, which looks like \"this app writes nothing\".",
		"err.probe.no_fuse_overlayfs":  "fuse-overlayfs is missing, so the overlay probe cannot run. Install it and retry (the strace fallback is not implemented in this build).",
		"err.probe.no_fusermount":      "fusermount3/fusermount is missing, so the probe mount point cannot be unmounted.",
		"err.probe.no_dev_fuse":        "/dev/fuse cannot be opened (missing or blocked by the sandbox), so the overlay cannot be mounted.",
		"err.probe.mount_failed":       "Mounting the probe write layer failed: {output}",
		"err.probe.umount_failed":      "Unmounting the probe write layer failed: {output}. The mount point may still be up; the next probe will hit EBUSY.",
		"err.probe.session":            "The probe session ended abnormally: {message}",
		"err.probe.start_failed":       "Cannot start the probe session ({exec}).",
		"err.probe.setsid_failed":      "Cannot give the probe session its own session (setsid failed); to avoid killing your terminal only the single process was stopped.",
		"err.probe.no_upper":           "The probe write layer {path} is missing or unreadable, so writes cannot be attributed.",
		"err.probe.workdir_in_home":    "The probe write layer {path} is inside your home ({home}): an overlay upper must not live inside its own lower, or the view recurses.",
		"err.probe.contaminated":       "This probe is void: writes appeared in shared folders ({paths}), so binding them back to the real location did not take effect and your files may have landed in the isolation layer. The scene is kept at {quarantine}; check it before deleting anything.",
		"err.probe.bind_verify_failed": "Inside the probe session, {path} still resolved to the isolation layer instead of the real folder. The shared-folder bind did not take effect, so this probe is void — the app had not started yet, so nothing was written.",
		"err.probe.no_user_dirs":       "Cannot read {path} (the XDG shared-folder list). The probe must know which folders are shared, otherwise it could delete your files; configure it once with your desktop's settings tool (or check XDG_CONFIG_HOME).",
		"err.probe.bad_public_dir":     "Shared folder {path} is not inside your home ({home}): binding it would cover the whole isolation view, so the probe would run against your real home. Fix user-dirs.dirs.",
		"err.probe.quarantine_failed":  "Cannot rename the probe scene into a sibling directory of {path}. To avoid deleting files that may be yours, appclone stopped here without deleting anything — please inspect that directory manually.",
		"err.probe.session_failed":     "The probe session could not start (exit code {code}). Usual causes: bwrap cannot create a user namespace, or the app's executable does not exist.",
		"err.probe.timeout":            "The probe timed out (the app was never closed). This result is incomplete, so it was voided and the scene kept.",
		"err.probe.stale_mount":        "A previous probe mount point {path} is still mounted and could not be unmounted; no new probe can start until it is gone.",
		"err.probe.unavailable":        "\"{app}\" has no usable isolation profile yet, and this build does not ship the probe pipeline. Wait for the probe feature, or use an app that ships a bundled template.",
		"err.probe.no_writes":          "No writes were observed for \"{app}\". Check that the app actually started (it may already be running, or lack a graphical session), then retry.",
		"err.audit.unavailable":        "The runtime audit layer (--audit) is not implemented yet in this build. Run without the flag; isolation itself is unaffected.",
		"ui.add.reconfigure_note":      "The profile for \"{app}\" was stale; rebuilding it from the bundled template. If the app's data directories moved, run Rediscover to correct it.",

		"probe.why.config":  "App config or data directory — isolate",
		"probe.why.data":    "Database/index file — isolate",
		"probe.why.cache":   "Cache — not isolated by default (slower first launch, but saves space)",
		"probe.why.public":  "Shared folder — must stay shared, cannot be isolated",
		"probe.why.shared":  "Shared writable — isolating breaks features (input method, bookmarks)",
		"probe.why.keyring": "Credentials live in the secret service, not on disk — cannot be isolated",
		"probe.why.dotdir":  "Directory written at the top of your home — please confirm it holds no files of your own",
		"probe.why.system":  "System path — should not be isolated (it would touch the host)",
		"probe.why.unknown": "Not covered by the rules — please confirm",

		"desktop.clone_name_suffix": " (Clone · {name})",

		"err.internal.read_profile":    "Reading profile {path} failed.",
		"err.internal.read_instance":   "Reading clone metadata {path} failed.",
		"err.internal.encode_profile":  "Encoding profile failed (internal error).",
		"err.internal.encode_instance": "Encoding clone metadata failed (internal error).",
		"err.internal.no_resolver":     "Internal error: missing path resolver.",
		"err.internal.unknown":         "Internal error: {message}",
		"err.internal.usage":           "Usage error: {message}",
		"err.internal.bind_escape":     "Internal error: bind path escaped the instance directory ({root} / {path}).",
	},
}
