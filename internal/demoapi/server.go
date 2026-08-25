package demoapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/jsonapi"
)

// Path prefixes the server answers under. The API and registry prefixes are
// go-tfe's own defaults; the blob prefix stands in for the artifact host a real
// deployment signs download URLs against.
const (
	apiPrefix      = "/api/v2/"
	registryPrefix = "/api/registry/private/v2/"
	blobPrefix     = "/blobs/"
	pingPath       = apiPrefix + "ping"
)

// mediaType is the JSON:API content type every document carries.
const mediaType = "application/vnd.api+json"

// Pagination bounds, matching the platform's own: twenty items unless asked
// otherwise, and never more than a hundred.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// shutdownGrace bounds how long a stopping server waits for in-flight requests
// before it closes their connections.
const shutdownGrace = 5 * time.Second

// Defaults for the served organization's history, sized for the archive the
// browsing recordings read.
const (
	// DefaultRuns is how many runs each workspace's history holds.
	DefaultRuns = 28
	// DefaultStates is how many state versions each workspace's history holds.
	DefaultStates = 40
	// DefaultSeed salts every identifier the organization is known by.
	DefaultSeed = "demo"
)

// config holds the resolved settings one [Server] serves from.
type config struct {
	seed   string
	runs   int
	states int
	chaos  bool
}

// Option configures a [Server] passed to [New].
//
// Options of this type:
//   - [WithSeed]
//   - [WithRuns]
//   - [WithStates]
//   - [WithChaos]
type Option func(*config)

// WithSeed salts every identifier the served organization is known by, so two
// servers started with the same seed serve the same organization and two
// started with different seeds serve disjoint ones. An empty seed keeps
// [DefaultSeed]. It returns an [Option].
func WithSeed(seed string) Option {
	return func(c *config) {
		if seed != "" {
			c.seed = seed
		}
	}
}

// WithRuns sets how many runs each workspace's history holds. A non-positive
// count keeps [DefaultRuns]. It returns an [Option].
func WithRuns(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.runs = n
		}
	}
}

// WithStates sets how many state versions each workspace's history holds. A
// non-positive count keeps [DefaultStates]. It returns an [Option].
func WithStates(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.states = n
		}
	}
}

// WithChaos turns the deterministic failure injector on, so the run a recording
// shows meets latency, one rate-limited response, a truncated download, and a
// single object that answers 404 before it answers bytes. It returns an
// [Option].
func WithChaos(enabled bool) Option {
	return func(c *config) {
		c.chaos = enabled
	}
}

// Server answers the slice of the HCP Terraform API the archiver reads.
//
// It serves one fictional organization, generated from the server's seed, over
// endpoints shaped closely enough that an archive collected from it is
// indistinguishable from one collected from the real platform. It is a
// documentation tool, not a fake for tests of the archiver's behavior: it
// tolerates any non-empty token, ignores most query parameters, and answers
// only the endpoints a default archive run reads.
//
// Create instances with [New].
type Server struct {
	chaos *injector
	cfg   config
}

// New creates a new [Server].
func New(opts ...Option) *Server {
	cfg := config{
		seed:   DefaultSeed,
		runs:   DefaultRuns,
		states: DefaultStates,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return &Server{
		cfg:   cfg,
		chaos: newInjector(cfg.chaos, cfg.seed, nil),
	}
}

// RateLimited reports how many requests the failure injector has answered with
// a rate-limit response, which is the tally the run's progress panel shows.
func (s *Server) RateLimited() int {
	return s.chaos.RateLimited()
}

// Serve answers requests on ln until ctx is done, then shuts down gracefully.
//
// Serve builds the organization rather than [New], because its artifact
// download URLs must be absolute and only a bound listener knows the address
// they resolve against.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	w, err := newWorld(s.cfg, "http://"+ln.Addr().String())
	if err != nil {
		return fmt.Errorf("build world: %w", err)
	}

	a := &api{world: w}
	s.chaos.retarget(w.chaosTargets())

	srv := &http.Server{
		Handler:           authorize(s.disrupt(a.routes())),
		ReadHeaderTimeout: 10 * time.Second,
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		<-ctx.Done()

		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()

		//nolint:contextcheck // The stopping context is deliberately detached from the canceled one.
		serr := srv.Shutdown(stopCtx)
		if serr != nil {
			slog.WarnContext(stopCtx, "demoapi shutdown incomplete", slog.Any("error", serr))
		}
	}()

	err = srv.Serve(ln)

	<-done

	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// authorize rejects a request that carries no bearer token.
//
// Any non-empty token is accepted: the ping a client opens with does not check
// the status, so a rejected token would not surface as a refused login but as
// hundreds of errored objects deep into a run.
func authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")

			return
		}

		next.ServeHTTP(w, r)
	})
}

// disrupt applies the failure injector's verdict to a request before the
// handler sees it.
func (s *Server) disrupt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := s.chaos.next(r.URL.Path)

		if v.latency > 0 {
			timer := time.NewTimer(v.latency)
			defer timer.Stop()

			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
		}

		switch {
		case v.status == http.StatusTooManyRequests:
			w.Header().Set("X-RateLimit-Reset", v.reset)
			writeError(w, http.StatusTooManyRequests, "rate limited")

		case v.status == http.StatusNotFound:
			writeError(w, http.StatusNotFound, "not found")

		case v.truncate:
			serveTruncated(w, r, next)

		default:
			next.ServeHTTP(w, r)
		}
	})
}

// serveTruncated runs the handler, then hands the client a response whose body
// stops short of the length it declared and whose connection closes behind it.
//
// A client reading it sees [io.ErrUnexpectedEOF], the ordinary shape of a
// transfer a network interrupted, which the archiver classifies transient and
// retries in-run. A truncation that cannot hijack the connection falls back to
// the whole response, since a demo failure is never worth failing the run over.
func serveTruncated(w http.ResponseWriter, r *http.Request, next http.Handler) {
	buf := &bufferedWriter{header: http.Header{}, status: http.StatusOK}
	next.ServeHTTP(buf, r)

	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		buf.flush(w)

		return
	}

	defer conn.Close() //nolint:errcheck // The close is the point: it truncates the stream.

	body := buf.body

	var out bytes.Buffer

	// The declared length is the whole body's, which is what makes the short
	// write below read as an interrupted transfer; the handler's own header
	// gives way to it rather than being sent twice.
	buf.header.Del("Content-Length")

	fmt.Fprintf(&out, "HTTP/1.1 %d %s\r\n", buf.status, http.StatusText(buf.status))
	fmt.Fprintf(&out, "Content-Length: %d\r\n", len(body))

	err = buf.header.Write(&out)
	if err != nil {
		return
	}

	out.WriteString("\r\n")
	out.Write(body[:len(body)/2])

	// The connection closes on this function's return either way, which is what
	// truncates the stream, so neither write below has an outcome worth acting
	// on.
	rw.Write(out.Bytes()) //nolint:errcheck,gosec // A deliberately partial response.
	rw.Flush()            //nolint:errcheck,gosec // Best-effort flush before the close.
}

// bufferedWriter collects a handler's whole response so the truncating path can
// rewrite it. It implements [http.ResponseWriter].
type bufferedWriter struct {
	header http.Header
	body   []byte
	status int
}

// Header returns the response headers the handler is building.
func (b *bufferedWriter) Header() http.Header { return b.header }

// WriteHeader records the status the handler chose.
func (b *bufferedWriter) WriteHeader(status int) { b.status = status }

// Write appends to the buffered body.
func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.body = append(b.body, p...)

	return len(p), nil
}

// flush replays the buffered response onto w, the fallback when the connection
// could not be hijacked.
func (b *bufferedWriter) flush(w http.ResponseWriter) {
	for name, values := range b.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	w.WriteHeader(b.status)
	w.Write(b.body) //nolint:errcheck,gosec // Best-effort replay of a rendered response.
}

// pageWindow is the pagination object a listing's meta carries, which is how
// the archiver's pager learns a listing's extent. A listing served without it
// stops at its first page and records itself complete, which is a silent
// truncation of the archive.
type pageWindow struct {
	CurrentPage  int `json:"current-page"`
	PreviousPage int `json:"prev-page"`
	NextPage     int `json:"next-page"`
	TotalCount   int `json:"total-count"`
	TotalPages   int `json:"total-pages"`
}

// windowFor returns the pagination object describing page number of size items
// out of total.
//
// TotalCount is the whole listing's, never the page's: the archiver probes a
// listing's size with a one-item page and treats a later, smaller count as
// evidence of deletion.
func windowFor(total, number, size int) pageWindow {
	if size < 1 {
		size = defaultPageSize
	}

	pages := max((total+size-1)/size, 1)
	number = min(max(number, 1), pages)

	win := pageWindow{
		CurrentPage: number,
		TotalCount:  total,
		TotalPages:  pages,
	}

	if number > 1 {
		win.PreviousPage = number - 1
	}

	if number < pages {
		win.NextPage = number + 1
	}

	return win
}

// pageOf returns the slice of items the window names.
func pageOf[T any](items []T, win pageWindow, size int) []T {
	if size < 1 {
		size = defaultPageSize
	}

	start := min((win.CurrentPage-1)*size, len(items))
	end := min(start+size, len(items))

	return items[start:end]
}

// pageParams reads the requested page number and size, clamped to the sizes the
// platform serves.
func pageParams(r *http.Request) (int, int) {
	number := intParam(r, "page[number]", 1)
	size := intParam(r, "page[size]", defaultPageSize)

	return max(number, 1), min(max(size, 1), maxPageSize)
}

// intParam reads a positive integer query parameter, falling back to fallback
// when it is missing or unreadable.
func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}

	return v
}

// writeList writes one page of items as a JSON:API list document carrying the
// pagination object the archiver's pager reads.
func writeList[T any](w http.ResponseWriter, r *http.Request, items []T) {
	number, size := pageParams(r)
	win := windowFor(len(items), number, size)

	payload, err := jsonapi.Marshal(pageOf(items, win, size))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal list: "+err.Error())

		return
	}

	many, ok := payload.(*jsonapi.ManyPayload)
	if !ok {
		writeError(w, http.StatusInternalServerError, "list payload was not a collection")

		return
	}

	many.Meta = &jsonapi.Meta{"pagination": win}
	sortNodes(many.Included)

	writeDocument(w, http.StatusOK, payload)
}

// writeOne writes a single object as a JSON:API document, sideloading whatever
// relations the model hydrates.
func writeOne(w http.ResponseWriter, model any) {
	payload, err := jsonapi.Marshal(model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "marshal object: "+err.Error())

		return
	}

	one, ok := payload.(*jsonapi.OnePayload)
	if ok {
		sortNodes(one.Included)
	}

	writeDocument(w, http.StatusOK, payload)
}

// sortNodes orders sideloaded resources by type and identifier, so two
// identical requests answer with identical bytes; the marshaler collects them
// through a map, whose iteration order is not.
func sortNodes(nodes []*jsonapi.Node) {
	slices.SortFunc(nodes, func(a, b *jsonapi.Node) int {
		if a.Type != b.Type {
			return strings.Compare(a.Type, b.Type)
		}

		return strings.Compare(a.ID, b.ID)
	})
}

// writeDocument encodes a JSON:API payload at the given status.
func writeDocument(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)

	err := encodeJSON(w, payload)
	if err != nil {
		slog.Warn("demoapi response write failed", slog.Any("error", err))
	}
}

// writeError answers with a JSON:API error document. The archiver reads the
// status, not the body, but a well-formed body keeps the exchange faithful.
func writeError(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(status)

	err := encodeJSON(w, errorDocument{Errors: []apiError{{
		Status: strconv.Itoa(status),
		Title:  title,
	}}})
	if err != nil {
		slog.Warn("demoapi error write failed", slog.Any("error", err))
	}
}

// errorDocument is a JSON:API error payload.
type errorDocument struct {
	Errors []apiError `json:"errors"`
}

// apiError is one error in an [errorDocument].
type apiError struct {
	Status string `json:"status"`
	Title  string `json:"title"`
}

// encodeJSON writes payload as JSON.
func encodeJSON(w http.ResponseWriter, payload any) error {
	err := json.NewEncoder(w).Encode(payload)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}

	return nil
}

// writeBlob answers with a raw artifact.
func writeBlob(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)

	_, err := w.Write(data)
	if err != nil {
		slog.Warn("demoapi blob write failed", slog.Any("error", err))
	}
}
