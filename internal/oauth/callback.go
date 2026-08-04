package oauth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// callbackResult carries the outcome of the single OAuth redirect back to the
// waiting CLI goroutine — either an authorization code or an error.
type callbackResult struct {
	code string
	err  error
}

// CallbackServer is a one-shot local HTTP server that listens on an ephemeral
// loopback port for Google's OAuth redirect. Desktop-type OAuth clients permit
// any 127.0.0.1:<port> redirect URI, so we let the OS choose a free port and
// build the redirect URI from it.
type CallbackServer struct {
	listener net.Listener
	server   *http.Server
	results  chan callbackResult
	state    string
}

// NewCallbackServer binds a loopback listener on an OS-chosen port. The caller
// must later call RedirectURI() to learn the address to register with Google,
// and Wait() to block for the redirect. state is the CSRF value that the
// incoming callback must echo back.
func NewCallbackServer(state string) (*CallbackServer, error) {
	// ":0" on the loopback interface → kernel assigns a free ephemeral port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind loopback listener: %w", err)
	}
	cs := &CallbackServer{
		listener: l,
		results:  make(chan callbackResult, 1),
		state:    state,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cs.handleCallback)
	cs.server = &http.Server{Handler: mux}
	return cs, nil
}

// RedirectURI returns the exact redirect_uri to send to Google, including the
// chosen port. Must match what the authorize request declares.
func (cs *CallbackServer) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", cs.listener.Addr().(*net.TCPAddr).Port)
}

// Start begins serving in the background. It returns immediately.
func (cs *CallbackServer) Start() {
	go func() {
		// Serve returns ErrServerClosed on graceful shutdown; that's expected.
		_ = cs.server.Serve(cs.listener)
	}()
}

// handleCallback processes the single redirect. It validates state, extracts
// either the code or an OAuth error, renders a human-facing page, and pushes
// the result to the waiting goroutine.
func (cs *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// An OAuth error (user denied consent, etc.) comes back as ?error=...
	if oauthErr := q.Get("error"); oauthErr != "" {
		cs.finish(w, false, fmt.Errorf("authorization denied: %s", oauthErr))
		return
	}

	// CSRF defense: the returned state must match what we generated.
	if q.Get("state") != cs.state {
		cs.finish(w, false, fmt.Errorf("state mismatch — possible CSRF, aborting"))
		return
	}

	code := q.Get("code")
	if code == "" {
		cs.finish(w, false, fmt.Errorf("no authorization code in callback"))
		return
	}

	cs.finish(w, true, nil)
	cs.results <- callbackResult{code: code}
}

// finish renders the browser-facing page and, on failure, also signals the
// waiting goroutine. On success the caller sends the code after this returns.
func (cs *CallbackServer) finish(w http.ResponseWriter, ok bool, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if ok {
		fmt.Fprint(w, successPage)
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, failurePage, err)
	cs.results <- callbackResult{err: err}
}

// Wait blocks until the callback arrives, the timeout elapses, or ctx is
// cancelled, then shuts the server down. Returns the authorization code.
func (cs *CallbackServer) Wait(ctx context.Context, timeout time.Duration) (string, error) {
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cs.server.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-cs.results:
		return res.code, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for browser authorization after %s — run `koolbase login` to try again", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

const successPage = `<!doctype html><html><head><meta charset="utf-8"><title>Koolbase CLI</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: dark; }
  html, body { margin: 0; height: 100%; }
  body {
    background: #0A0A0B;
    color: #f1f5f9;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    display: flex; align-items: center; justify-content: center; min-height: 100vh;
  }
  .card { text-align: center; padding: 48px 32px; max-width: 420px; }
  .logo { display: flex; align-items: center; justify-content: center; gap: 10px; margin-bottom: 32px; }
  .logo span { font-weight: 700; color: #fff; letter-spacing: -0.02em; font-size: 18px; }
  .logo .docs { color: #64748b; font-weight: 500; font-size: 13px; font-family: ui-monospace, monospace; }
  .check {
    width: 48px; height: 48px; border-radius: 999px;
    background: rgba(52,211,153,0.12); border: 1px solid rgba(52,211,153,0.3);
    display: flex; align-items: center; justify-content: center; margin: 0 auto 24px;
  }
  h1 { color: #fff; font-size: 22px; font-weight: 700; letter-spacing: -0.02em; margin: 0 0 10px; }
  p { color: #94a3b8; font-size: 15px; line-height: 1.6; margin: 0; }
</style></head>
<body>
  <div class="card">
    <div class="logo">
      <svg width="26" height="26" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
        <rect width="32" height="32" rx="8" fill="#3B82F6"/>
        <path d="M11 8v16M11 16l8-8M11 16l8 8" stroke="#0A0A0B" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
      <span>Koolbase</span><span class="docs">CLI</span>
    </div>
    <div class="check">
      <svg width="24" height="24" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M5 13l4 4L19 7" stroke="#34d399" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    </div>
    <h1>You're signed in</h1>
    <p>You can close this tab and return to your terminal.</p>
  </div>
</body></html>`

const failurePage = `<!doctype html><html><head><meta charset="utf-8"><title>Koolbase CLI</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { color-scheme: dark; }
  html, body { margin: 0; height: 100%%; }
  body {
    background: #0A0A0B;
    color: #f1f5f9;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    display: flex; align-items: center; justify-content: center; min-height: 100vh;
  }
  .card { text-align: center; padding: 48px 32px; max-width: 420px; }
  .logo { display: flex; align-items: center; justify-content: center; gap: 10px; margin-bottom: 32px; }
  .logo span { font-weight: 700; color: #fff; letter-spacing: -0.02em; font-size: 18px; }
  .logo .docs { color: #64748b; font-weight: 500; font-size: 13px; font-family: ui-monospace, monospace; }
  .cross {
    width: 48px; height: 48px; border-radius: 999px;
    background: rgba(248,113,113,0.12); border: 1px solid rgba(248,113,113,0.3);
    display: flex; align-items: center; justify-content: center; margin: 0 auto 24px;
  }
  h1 { color: #fff; font-size: 22px; font-weight: 700; letter-spacing: -0.02em; margin: 0 0 10px; }
  p { color: #94a3b8; font-size: 15px; line-height: 1.6; margin: 0 0 6px; }
  .reason { color: #f87171; font-size: 13px; font-family: ui-monospace, monospace; margin-top: 12px; word-break: break-word; }
</style></head>
<body>
  <div class="card">
    <div class="logo">
      <svg width="26" height="26" viewBox="0 0 32 32" fill="none" xmlns="http://www.w3.org/2000/svg">
        <rect width="32" height="32" rx="8" fill="#3B82F6"/>
        <path d="M11 8v16M11 16l8-8M11 16l8 8" stroke="#0A0A0B" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
      <span>Koolbase</span><span class="docs">cli</span>
    </div>
    <div class="cross">
      <svg width="24" height="24" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
        <path d="M7 7l10 10M17 7L7 17" stroke="#f87171" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    </div>
    <h1>Sign-in failed</h1>
    <p>Return to your terminal and try again.</p>
    <p class="reason">%v</p>
  </div>
</body></html>`
