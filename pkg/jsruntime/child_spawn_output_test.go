package jsruntime

import (
	"io"
	"runtime"
	"testing"
	"time"
)

func TestChildSpawnCloseWaitsForBothStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	r, err := New(`
        async function verify() {
            const cp = require("child_process");
            for (let attempt = 0; attempt < 10; attempt++) {
                const ok = await new Promise((resolve) => {
                    const child = cp.spawn("printf stdout; printf stderr >&2", [], {shell: true, encoding: "utf8"});
                    let stdout = "", stderr = "", outEnded = false, errEnded = false;
                    child.stdout.on("data", (data) => stdout += String(data));
                    child.stderr.on("data", (data) => stderr += String(data));
                    child.stdout.on("end", () => outEnded = true);
                    child.stderr.on("end", () => errEnded = true);
                    child.on("close", (code) => resolve(code === 0 && stdout === "stdout" && stderr === "stderr" && outEnded && errEnded));
                    child.on("error", () => resolve(false));
                });
                if (!ok) return false;
            }
            return true;
        }
    `, Options{NodeJS: true, AllowExec: true, BaseDir: t.TempDir(), Console: io.Discard, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Call("verify"); err != nil {
		t.Fatalf("child output/close ordering: %v", err)
	}
}
