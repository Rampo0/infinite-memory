package setup

import (
	"fmt"
	"path/filepath"
)

const LaunchdLabel = "com.ammar.imemd"

func Plist(home, exe, extraPath string) string {
	state := filepath.Join(home, ".local", "state", "infinite-memory")
	path := "/usr/local/bin:/usr/bin:/bin:" + filepath.Join(home, ".local", "bin")
	if extraPath != "" {
		path = extraPath + ":" + path
	}
	return fmt.Sprintf(plistTemplate, LaunchdLabel, exe, filepath.Join(state, "launchd.out.log"),
		filepath.Join(state, "launchd.err.log"), path)
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`
