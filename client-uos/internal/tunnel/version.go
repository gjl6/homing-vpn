package tunnel

// version 会被 main 包通过 SetVersion 设置（也可用 -ldflags -X 注入）。
var version = "1.0.0"

// SetVersion 设置对外展示的版本号。
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}
