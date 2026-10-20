package jsruntime

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/require"
)

func TestNewReturnsRuntimeWithInjectedGlobals(t *testing.T) {
	runtime, err := New(`
		function sendMessage() {
			return typeof console.assert === "function" &&
				typeof console.debug === "function" &&
				typeof console.error === "function" &&
				typeof console.exception === "function" &&
				typeof console.info === "function" &&
				typeof console.log === "function" &&
				typeof console.trace === "function" &&
				typeof console.warn === "function" &&
				typeof fetch === "function" &&
				typeof XMLHttpRequest === "function" &&
				typeof Promise === "function" &&
				typeof require === "function" &&
				typeof queueMicrotask === "function" &&
				typeof setTimeout === "function" &&
				typeof clearTimeout === "function" &&
				typeof setInterval === "function" &&
				typeof clearInterval === "function" &&
				typeof setImmediate === "function";
        }
    `, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("injected globals are not ready: %v", err)
	}
}

func TestConsoleMethodsWriteMessagesAndStacks(t *testing.T) {
	var output bytes.Buffer
	runtime, err := New(`
        function sendMessage() {
            console.debug("debug");
            console.info("value=%d", 2);
            console.log("log", "message");
            console.warn("warning");
            console.error("error");
            console.exception("exception");
            console.assert(false, "assertion");
            console.trace("trace");
            return true;
        }
    `, Options{Console: &output, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"debug", "value=2", "log message", "warning", "error", "exception", "Assertion failed:", "assertion", "trace", "Error"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("console output does not contain %q: %s", expected, output.String())
		}
	}
}

func TestCallHandlesPromise(t *testing.T) {
	runtime, err := New(`
        function sendMessage(value) {
            return Promise.resolve(value === "ready");
        }
    `, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage", "ready"); err != nil {
		t.Fatalf("Promise call failed: %v", err)
	}
}

func TestNewDoesNotRequireSendMessage(t *testing.T) {
	runtime, err := New(`
		function ping() {
			return true;
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("ping"); err != nil {
		t.Fatalf("generic runtime call failed: %v", err)
	}
}

func TestRequireLoadsAndCachesCommonJSModule(t *testing.T) {
	modulePath := filepath.Join(t.TempDir(), "module.js")
	if err := os.WriteFile(modulePath, []byte(`
		globalThis.__moduleLoads = (globalThis.__moduleLoads || 0) + 1;
		module.exports = { value: 42 };
	`), 0o600); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
		function sendMessage() {
			const first = require(%q);
			const second = require(%q);
			return first === second && first.value === 42 && globalThis.__moduleLoads === 1;
		}
	`, filepath.ToSlash(modulePath), filepath.ToSlash(modulePath))
	runtime, err := New(script, Options{Console: io.Discard, Timeout: time.Second, BaseDir: filepath.Dir(modulePath)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("CommonJS require failed: %v", err)
	}
}

func TestRequireDefaultsToCurrentDirectoryWhenBaseDirEmpty(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "base")
	if err := os.Mkdir(baseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "inside.js"), []byte(`module.exports = "inside";`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.js"), []byte(`module.exports = "outside";`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(baseDir)

	runtime, err := New(`
		function sendMessage() {
			let denied = false;
			try { require("../outside.js"); } catch (error) { denied = String(error).includes("escapes"); }
			return require("./inside.js") === "inside" && denied;
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("implicit current-directory BaseDir confinement failed: %v", err)
	}
}

func TestRequireResolvesRelativeModulesFromBaseDir(t *testing.T) {
	baseDir := t.TempDir()
	nestedDir := filepath.Join(baseDir, "nested")
	if err := os.Mkdir(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "b.js"), []byte(`
		module.exports = { value: "from-base-dir" };
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "a.js"), []byte(`
		module.exports = require("./nested/b.js");
	`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := New(`
		function sendMessage() {
			return require("./a.js").value === "from-base-dir";
		}
	`, Options{
		Console: io.Discard,
		Timeout: time.Second,
		BaseDir: baseDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("BaseDir relative require failed: %v", err)
	}
}

func TestBaseDirValidationAndConfinement(t *testing.T) {
	root := t.TempDir()
	baseDir := filepath.Join(root, "plugin")
	if err := os.Mkdir(baseDir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("missing directory", func(t *testing.T) {
		_, err := New("void 0", Options{BaseDir: filepath.Join(root, "missing")})
		if err == nil {
			t.Fatal("expected missing BaseDir to fail")
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		filePath := filepath.Join(root, "file.js")
		if err := os.WriteFile(filePath, []byte("void 0"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := New("void 0", Options{BaseDir: filePath})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("expected non-directory BaseDir error, got %v", err)
		}
	})

	outsidePath := filepath.Join(root, "outside.js")
	if err := os.WriteFile(outsidePath, []byte("module.exports = true"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(`
		function loadOutside() {
			return require("../outside.js");
		}
	`, Options{BaseDir: baseDir, Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err := runtime.Call("loadOutside"); err == nil || !strings.Contains(err.Error(), "escapes BaseDir") {
		t.Fatalf("expected path traversal to fail, got %v", err)
	}

	t.Run("symlink escape", func(t *testing.T) {
		linkPath := filepath.Join(baseDir, "outside-link.js")
		if err := os.Symlink(outsidePath, linkPath); err != nil {
			t.Skipf("symlink creation is unavailable: %v", err)
		}
		linkRuntime, err := New(`
			function loadLink() {
				return require("./outside-link.js");
			}
		`, Options{BaseDir: baseDir, Console: io.Discard, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer linkRuntime.Close()
		if err := linkRuntime.Call("loadLink"); err == nil || !strings.Contains(err.Error(), "symlink escapes BaseDir") {
			t.Fatalf("expected symlink escape to fail, got %v", err)
		}
	})
}

func TestRequireProvidesSupportedCoreModules(t *testing.T) {
	runtime, err := New(`
		function sendMessage() {
			const Buffer = require("buffer").Buffer;
			const URL = require("node:url").URL;
			const util = require("util");
			return Buffer.from("ok").toString("hex") === "6f6b" &&
				new URL("https://example.com/path").pathname === "/path" &&
				util.format("value=%d", 2) === "value=2";
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("core module require failed: %v", err)
	}
}

func TestURLOriginPreservesPortAndFollowsWHATWG(t *testing.T) {
	runtime, err := New(`
		function verify() {
			function assert(cond, msg) {
				if (!cond) throw new Error(msg);
			}

			// 1. URL and URLSearchParams are globally available without require
			assert(typeof URL === "function", "1. URL global");
			assert(typeof URLSearchParams === "function", "1. URLSearchParams global");
			assert(typeof globalThis.URL === "function", "1. globalThis.URL");
			assert(typeof globalThis.URLSearchParams === "function", "1. globalThis.URLSearchParams");

			// 2. Non-default ports are preserved in URL.origin
			const u1 = new URL("http://127.0.0.1:8080/api/v1");
			assert(u1.origin === "http://127.0.0.1:8080", "2. u1.origin=" + u1.origin);

			// 3. Default port (80 for http) is omitted
			const u2 = new URL("http://127.0.0.1:80/api/v1");
			assert(u2.origin === "http://127.0.0.1", "3. u2.origin=" + u2.origin);

			// 4. Non-default HTTPS port is preserved
			const u3 = new URL("https://example.com:8443/test");
			assert(u3.origin === "https://example.com:8443", "4. u3.origin=" + u3.origin);

			// 5. Default HTTPS port (443) is omitted
			const u4 = new URL("https://example.com:443/test");
			assert(u4.origin === "https://example.com", "5. u4.origin=" + u4.origin);

			// 6. Standard URL without port
			const u5 = new URL("https://example.com/test");
			assert(u5.origin === "https://example.com", "6. u5.origin=" + u5.origin);

			// 7. ws and wss protocols
			const u6 = new URL("ws://127.0.0.1:9090/ws");
			assert(u6.origin === "ws://127.0.0.1:9090", "7. u6.origin=" + u6.origin);
			const u7 = new URL("wss://127.0.0.1:9443/ws");
			assert(u7.origin === "wss://127.0.0.1:9443", "7. u7.origin=" + u7.origin);

			// 8. ftp protocol
			const u8 = new URL("ftp://ftp.example.com:2121/");
			assert(u8.origin === "ftp://ftp.example.com:2121", "8. u8.origin=" + u8.origin);

			// 9. file: URL origin is "null"
			const u9 = new URL("file:///etc/hosts");
			assert(u9.origin === "null", "9. u9.origin=" + u9.origin);

			// 10. mailto: URL origin is "null"
			const u10 = new URL("mailto:admin@example.com");
			assert(u10.origin === "null", "10. u10.origin=" + u10.origin);

			// 11. blob: URL origin inherits inner URL
			const u11 = new URL("blob:http://127.0.0.1:8080/some-guid");
			assert(u11.origin === "http://127.0.0.1:8080", "11. u11.origin=" + u11.origin);

			// 12. Dynamic port mutations update origin
			const u12 = new URL("http://127.0.0.1:8080/foo");
			u12.port = "3000";
			assert(u12.origin === "http://127.0.0.1:3000", "12a. u12.origin=" + u12.origin);
			u12.port = "80";
			assert(u12.origin === "http://127.0.0.1", "12b. u12.origin=" + u12.origin);

			// 13. URL with username and password does NOT include userinfo in origin
			const u13 = new URL("http://user:pass@127.0.0.1:8080/foo");
			assert(u13.origin === "http://127.0.0.1:8080", "13. u13.origin=" + u13.origin);

			// 14. IPv6 with port
			const u14 = new URL("http://[::1]:8080/foo");
			assert(u14.origin === "http://[::1]:8080", "14. u14.origin=" + u14.origin);

			// 15. require("url") and require("node:url") return same URL and URLSearchParams constructors
			const urlMod = require("url");
			const nodeUrlMod = require("node:url");
			assert(urlMod.URL === URL, "15a. urlMod.URL === URL");
			assert(nodeUrlMod.URL === URL, "15b. nodeUrlMod.URL === URL");
			assert(urlMod.URLSearchParams === URLSearchParams, "15c. urlMod.URLSearchParams");
			assert(nodeUrlMod.URLSearchParams === URLSearchParams, "15d. nodeUrlMod.URLSearchParams");

			// 16. URL.canParse
			assert(typeof URL.canParse === "function", "16a. typeof URL.canParse");
			assert(URL.canParse("http://127.0.0.1:8080/test"), "16b. canParse valid");
			assert(URL.canParse("/foo", "http://127.0.0.1:8080"), "16c. canParse with base");
			assert(!URL.canParse("invalid url without base"), "16d. canParse invalid");

			// 17. Calling origin getter on plain object throws TypeError
			let threw = false;
			try {
				const desc = Object.getOwnPropertyDescriptor(URL.prototype, "origin");
				desc.get.call({});
			} catch (e) {
				threw = e instanceof TypeError;
			}
			assert(threw, "17. threw TypeError on plain object");

			return true;
		}
	`, Options{Console: io.Discard, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("verify"); err != nil {
		t.Fatalf("URL origin verification failed: %v", err)
	}
}

func TestRequireConfigurationComposesWithBaseDir(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "module.js"), []byte(`
		module.exports = { value: "local" };
	`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(`
		function sendMessage() {
			return require("komari").version === "test" &&
				require("./module.js").value === "local";
		}
	`, Options{
		BaseDir: baseDir,
		Console: io.Discard,
		Timeout: time.Second,
		ConfigureRequire: func(registry *require.Registry) {
			registry.RegisterNativeModule("komari", func(vm *goja.Runtime, module *goja.Object) {
				exports := module.Get("exports").ToObject(vm)
				_ = exports.Set("version", "test")
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("configured require with BaseDir failed: %v", err)
	}
}

func TestConfigureRequireCanOverrideNodeModule(t *testing.T) {
	runtime, err := New(`
		function verify() {
			return require("path").source === "custom";
		}
	`, Options{
		NodeJS:  true,
		BaseDir: t.TempDir(),
		Console: io.Discard,
		Timeout: time.Second,
		ConfigureRequire: func(registry *require.Registry) {
			registry.RegisterNativeModule("path", func(vm *goja.Runtime, module *goja.Object) {
				exports := module.Get("exports").ToObject(vm)
				_ = exports.Set("source", "custom")
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("verify"); err != nil {
		t.Fatalf("configured require did not override Node module: %v", err)
	}
}

func TestEventLoopRunsMicrotasksBeforeTimers(t *testing.T) {
	runtime, err := New(`
		function sendMessage() {
			const order = [];
			return new Promise((resolve) => {
				const cancelled = setTimeout(() => resolve(false), 1);
				clearTimeout(cancelled);
				setTimeout(() => {
					order.push("timer");
					resolve(order.join(",") === "sync,microtask,timer");
				}, 10);
				queueMicrotask(() => order.push("microtask"));
				order.push("sync");
			});
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err != nil {
		t.Fatalf("event loop ordering failed: %v", err)
	}
}

func TestTimerCallbackTimeoutDoesNotBlockEventLoop(t *testing.T) {
	var output bytes.Buffer
	runtime, err := New(`
		function startTimer() {
			setTimeout(() => {
				const end = Date.now() + 250;
				while (Date.now() < end) {}
			}, 0);
			return true;
		}
		function ping() {
			return true;
		}
	`, Options{Console: &output, Timeout: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("startTimer"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := runtime.Call("ping"); err != nil {
		t.Fatalf("timer callback blocked the event loop: %v", err)
	}
	if !strings.Contains(output.String(), "setTimeout callback failed") || !strings.Contains(output.String(), "timeout") {
		t.Fatalf("timer timeout was not reported: %s", output.String())
	}
}

func TestFetchPromiseResolvesOnEventLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Test") != "event-loop" {
			http.Error(response, "missing header", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"accepted":true}`)
	}))
	defer server.Close()

	runtime, err := New(`
		function sendMessage(url) {
			return fetch(url, { headers: { "X-Test": "event-loop" } })
				.then((response) => response.json())
				.then((body) => body.accepted === true);
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage", server.URL); err != nil {
		t.Fatalf("fetch Promise failed: %v", err)
	}
}

func TestAsyncXHRCallbackRunsOnEventLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("X-Runtime", "jsruntime")
		_, _ = io.WriteString(response, "ready")
	}))
	defer server.Close()

	runtime, err := New(`
		function sendMessage(url) {
			return new Promise((resolve) => {
				const request = new XMLHttpRequest();
				request.open("GET", url, true);
				request.onload = () => resolve(
					request.status === 200 &&
					request.responseText === "ready" &&
					request.getResponseHeader("X-Runtime") === "jsruntime"
				);
				request.onerror = () => resolve(false);
				request.send();
			});
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage", server.URL); err != nil {
		t.Fatalf("asynchronous XMLHttpRequest failed: %v", err)
	}
}

func TestCallInterruptsInfiniteLoopAndRuntimeRemainsUsable(t *testing.T) {
	runtime, err := New(`
		function sendMessage(block) {
			if (block) {
				for (;;) {}
			}
			return Promise.resolve(true);
		}
	`, Options{Console: io.Discard, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage", true); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if err := runtime.Call("sendMessage", false); err != nil {
		t.Fatalf("runtime was not reusable after interruption: %v", err)
	}
}

func TestPromiseRejectionIsReturned(t *testing.T) {
	runtime, err := New(`
		function sendMessage() {
			return new Promise((resolve, reject) => {
				setTimeout(() => reject(new Error("nope")), 1);
			});
		}
	`, Options{Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.Call("sendMessage"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected Promise rejection, got %v", err)
	}
}

func TestConfigureHostProvidesHostServicesAndModules(t *testing.T) {
	var host *Host
	runtime, err := New(`
		function run() {
			return require("server").name === "test-server";
		}
	`, Options{
		BaseDir: t.TempDir(),
		Console: io.Discard,
		Timeout: time.Second,
		ConfigureHost: func(h *Host, registry *require.Registry) {
			host = h
			registry.RegisterNativeModule("server", func(vm *goja.Runtime, module *goja.Object) {
				exports := module.Get("exports").ToObject(vm)
				_ = exports.Set("name", "test-server")
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if host == nil {
		t.Fatal("ConfigureHost was not called")
	}
	if err := runtime.Call("run"); err != nil {
		t.Fatalf("host module not available to script: %v", err)
	}

	// Host services must be usable from another goroutine while the runtime
	// is idle: queue a job and wait for it to run on the event loop.
	ran := make(chan bool, 1)
	if !host.RunOnLoop(func(vm *goja.Runtime) {
		ran <- true
	}) {
		t.Fatal("RunOnLoop rejected a job on an open runtime")
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnLoop job did not run")
	}
}

func TestCallVoidIgnoresFalsyResult(t *testing.T) {
	runtime, err := New(`
		function sideEffect() {
			console.log("ran");
		}
	`, Options{BaseDir: t.TempDir(), Console: io.Discard, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	if err := runtime.CallVoid("sideEffect"); err != nil {
		t.Fatalf("CallVoid rejected a falsy result: %v", err)
	}
	if err := runtime.Call("sideEffect"); err == nil {
		t.Fatal("Call must still reject a falsy result")
	}
}
