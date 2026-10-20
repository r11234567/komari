package url

import (
	"fmt"
	"sync"

	"github.com/dop251/goja"
	goja_url "github.com/dop251/goja_nodejs/url"
)

// Module wraps goja_nodejs/url to fix WHATWG URL compliance issues such as
// preserving non-default ports in URL.origin.
type Module struct {
	mu      sync.Mutex
	exports *goja.Object
}

// New creates a new url Module instance for a Runtime.
func New() *Module {
	return &Module{}
}

// Load loads the url module for CommonJS require("url") or require("node:url").
func (m *Module) Load(vm *goja.Runtime, module *goja.Object) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.exports != nil {
		_ = module.Set("exports", m.exports)
		return
	}

	goja_url.Require(vm, module)
	exportsVal := module.Get("exports")
	if exportsVal != nil && !goja.IsUndefined(exportsVal) && !goja.IsNull(exportsVal) {
		exports := exportsVal.ToObject(vm)
		patchURL(vm, exports)
		m.exports = exports
	}
}

// Inject exposes URL and URLSearchParams globally on the runtime.
func (m *Module) Inject(vm *goja.Runtime) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.exports == nil {
		mod := vm.NewObject()
		mod.Set("exports", vm.NewObject())
		goja_url.Require(vm, mod)
		exportsVal := mod.Get("exports")
		if exportsVal != nil && !goja.IsUndefined(exportsVal) && !goja.IsNull(exportsVal) {
			exports := exportsVal.ToObject(vm)
			patchURL(vm, exports)
			m.exports = exports
		}
	}

	if m.exports != nil {
		if err := vm.Set("URL", m.exports.Get("URL")); err != nil {
			return err
		}
		if err := vm.Set("URLSearchParams", m.exports.Get("URLSearchParams")); err != nil {
			return err
		}
	}
	return nil
}

const patchScript = `(function(URL) {
	if (!URL || typeof URL !== "function" || !URL.prototype) {
		return;
	}
	var origProto = URL.prototype;
	var newProto = Object.create(Object.getPrototypeOf(origProto));
	var propNames = Object.getOwnPropertyNames(origProto);
	var hrefDesc = Object.getOwnPropertyDescriptor(origProto, "href");
	for (var i = 0; i < propNames.length; i++) {
		var name = propNames[i];
		if (name === "origin") {
			Object.defineProperty(newProto, name, {
				get: function() {
					if (hrefDesc && hrefDesc.get) {
						hrefDesc.get.call(this);
					}
					var protocol = this.protocol;
					if (!protocol || protocol === "file:") {
						return "null";
					}
					if (protocol === "blob:") {
						try {
							var inner = this.pathname || (this.href ? this.href.replace(/^blob:/, "") : "");
							return new URL(inner).origin;
						} catch (e) {
							return "null";
						}
					}
					switch (protocol) {
					case "http:":
					case "https:":
					case "ws:":
					case "wss:":
					case "ftp:":
						var host = this.host;
						if (host) {
							return protocol + "//" + host;
						}
						return "null";
					default:
						return "null";
					}
				},
				configurable: true,
				enumerable: false
			});
		} else {
			var desc = Object.getOwnPropertyDescriptor(origProto, name);
			if (desc) {
				Object.defineProperty(newProto, name, desc);
			}
		}
	}
	URL.prototype = newProto;

	if (typeof URL.canParse !== "function") {
		URL.canParse = function canParse(url, base) {
			if (url === undefined || url === null) {
				return false;
			}
			try {
				if (base !== undefined) {
					new URL(url, base);
				} else {
					new URL(url);
				}
				return true;
			} catch (e) {
				return false;
			}
		};
	}
})`

func patchURL(vm *goja.Runtime, exports *goja.Object) {
	urlVal := exports.Get("URL")
	if urlVal == nil || goja.IsUndefined(urlVal) || goja.IsNull(urlVal) {
		return
	}
	patchFn, err := vm.RunString(patchScript)
	if err != nil {
		panic(fmt.Errorf("compile URL patch script: %w", err))
	}
	if fn, ok := goja.AssertFunction(patchFn); ok {
		if _, err := fn(goja.Undefined(), urlVal); err != nil {
			panic(fmt.Errorf("execute URL patch: %w", err))
		}
	}
}
