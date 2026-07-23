package public

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const historyClientModule = `class KomariHistoryClient {
  constructor(endpoint = "/api/v1/history/query") {
    this.endpoint = endpoint;
    this.controllers = new Map();
  }

  cancel(key = "default") {
    this.controllers.get(key)?.abort();
    this.controllers.delete(key);
  }

  async query(request, options = {}) {
    const key = options.key ?? "default";
    const timeout = options.timeout ?? 25000;
    this.cancel(key);
    const controller = new AbortController();
    this.controllers.set(key, controller);
    const timer = setTimeout(() => controller.abort(new DOMException("History query timed out", "TimeoutError")), timeout);
    try {
      const response = await fetch(this.endpoint, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(request),
        signal: controller.signal,
      });
      const body = await response.json().catch(() => ({}));
      if (!response.ok) {
        const error = new Error(body.message ?? ("History query failed (" + response.status + ")"));
        error.code = body.code;
        error.status = response.status;
        throw error;
      }
      return body;
    } finally {
      clearTimeout(timer);
      if (this.controllers.get(key) === controller) this.controllers.delete(key);
    }
  }

  dispose() {
    for (const controller of this.controllers.values()) controller.abort();
    this.controllers.clear();
  }
}

export { KomariHistoryClient };
export const historyClient = new KomariHistoryClient();
`

func HistoryClientModule(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", []byte(historyClientModule))
}
