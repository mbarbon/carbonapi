package main

import (
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/dgryski/httputil"
	"github.com/facebookgo/grace/gracehttp"
	"github.com/facebookgo/pidfile"
	"github.com/go-graphite/carbonapi/intervalset"
	"github.com/go-graphite/carbonapi/mstats"
	"github.com/go-graphite/carbonapi/pathcache"
	"github.com/go-graphite/carbonapi/util"
	"github.com/go-graphite/carbonapi/zipper"
	pb3 "github.com/go-graphite/protocol/carbonapi_v2_pb"
	pickle "github.com/lomik/og-rek"
	"github.com/peterbourgon/g2g"

	"github.com/lomik/zapwriter"
	"github.com/satori/go.uuid"
	"go.uber.org/zap"
)

var defaultLoggerConfig = zapwriter.Config{
	Logger:           "",
	File:             "stdout",
	Level:            "info",
	Encoding:         "console",
	EncodingTime:     "iso8601",
	EncodingDuration: "seconds",
}

// GraphiteConfig contains configuration bits to send internal stats to Graphite
type GraphiteConfig struct {
	Pattern  string
	Host     string
	Interval time.Duration
	Prefix   string
}

// config contains necessary information for global
var config = struct {
	Backends []string       `yaml:"backends"`
	MaxProcs int            `yaml:"maxProcs"`
	Graphite GraphiteConfig `yaml:"graphite"`
	Listen   string         `yaml:"listen"`
	Buckets  int            `yaml:"buckets"`

	Timeouts          zipper.Timeouts `yaml:"timeouts"`
	KeepAliveInterval time.Duration   `yaml:"keepAliveInterval"`

	CarbonSearch zipper.CarbonSearch `yaml:"carbonsearch"`

	MaxIdleConnsPerHost int `yaml:"maxIdleConnsPerHost"`

	ConcurrencyLimitPerServer  int                `yaml:"concurrencyLimit"`
	ExpireDelaySec             int32              `yaml:"expireDelaySec"`
	Logger                     []zapwriter.Config `yaml:"logger"`
	GraphiteWeb09Compatibility bool               `yaml:"graphite09compat"`

	zipper *zipper.Zipper
}{
	MaxProcs: 1,
	Graphite: GraphiteConfig{
		Interval: 60 * time.Second,
		Prefix:   "carbon.zipper",
		Pattern:  "{prefix}.{fqdn}",
	},
	Listen:  ":8080",
	Buckets: 10,

	Timeouts: zipper.Timeouts{
		Global:       10000 * time.Millisecond,
		AfterStarted: 2 * time.Second,
		Connect:      200 * time.Millisecond,
	},
	KeepAliveInterval: 30 * time.Second,

	MaxIdleConnsPerHost: 100,

	ExpireDelaySec: 10 * 60, // 10 minutes

	Logger: []zapwriter.Config{defaultLoggerConfig},
}

// Metrics contains grouped expvars for /debug/vars and graphite
var Metrics = struct {
	Requests  *expvar.Int
	Responses *expvar.Int
	Errors    *expvar.Int

	Goroutines expvar.Func

	FindRequests *expvar.Int
	FindErrors   *expvar.Int

	SearchRequests *expvar.Int

	RenderRequests *expvar.Int
	RenderErrors   *expvar.Int

	InfoRequests *expvar.Int
	InfoErrors   *expvar.Int

	Timeouts *expvar.Int

	CacheSize         expvar.Func
	CacheItems        expvar.Func
	CacheMisses       *expvar.Int
	CacheHits         *expvar.Int
	SearchCacheSize   expvar.Func
	SearchCacheItems  expvar.Func
	SearchCacheMisses *expvar.Int
	SearchCacheHits   *expvar.Int
}{
	Requests:  expvar.NewInt("requests"),
	Responses: expvar.NewInt("responses"),
	Errors:    expvar.NewInt("errors"),

	FindRequests: expvar.NewInt("find_requests"),
	FindErrors:   expvar.NewInt("find_errors"),

	SearchRequests: expvar.NewInt("search_requests"),

	RenderRequests: expvar.NewInt("render_requests"),
	RenderErrors:   expvar.NewInt("render_errors"),

	InfoRequests: expvar.NewInt("info_requests"),
	InfoErrors:   expvar.NewInt("info_errors"),

	Timeouts: expvar.NewInt("timeouts"),

	CacheHits:         expvar.NewInt("cache_hits"),
	CacheMisses:       expvar.NewInt("cache_misses"),
	SearchCacheHits:   expvar.NewInt("search_cache_hits"),
	SearchCacheMisses: expvar.NewInt("search_cache_misses"),
}

// BuildVersion is defined at build and reported at startup and as expvar
var BuildVersion = "(development version)"

// set during startup, read-only after that
var searchConfigured = false

const (
	contentTypeJSON     = "application/json"
	contentTypeProtobuf = "application/x-protobuf"
	contentTypePickle   = "application/pickle"
)

const (
	formatTypeEmpty         = ""
	formatTypePickle        = "pickle"
	formatTypeJSON          = "json"
	formatTypeProtobuf      = "protobuf"
	formatTypeProtobuf3     = "protobuf3"
	formatTypeV2            = "v2"
	formatTypeCarbonAPIV2PB = "carbonapi_v2_pb"
)

func findHandler(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	uuid := uuid.NewV4()
	ctx := req.Context()
	ctx = util.SetUUID(ctx, uuid.String())
	logger := zapwriter.Logger("find").With(
		zap.String("handler", "find"),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)
	logger.Debug("got find request",
		zap.String("request", req.URL.RequestURI()),
	)

	originalQuery := req.FormValue("query")
	format := req.FormValue("format")

	Metrics.Requests.Add(1)
	Metrics.FindRequests.Add(1)

	accessLogger := zapwriter.Logger("access").With(
		zap.String("handler", "find"),
		zap.String("format", format),
		zap.String("target", originalQuery),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)

	metrics, stats, err := config.zipper.Find(ctx, logger, originalQuery)
	sendStats(stats)
	if err != nil {
		accessLogger.Error("find failed",
			zap.Int("http_code", http.StatusInternalServerError),
			zap.String("reason", err.Error()),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		http.Error(w, "error fetching the data", http.StatusInternalServerError)
		Metrics.Errors.Add(1)
		return
	}

	err = encodeFindResponse(format, originalQuery, w, metrics)
	if err != nil {
		http.Error(w, "error marshaling data", http.StatusInternalServerError)
		accessLogger.Error("render failed",
			zap.Int("http_code", http.StatusInternalServerError),
			zap.String("reason", "error marshaling data"),
			zap.Duration("runtime_seconds", time.Since(t0)),
			zap.Error(err),
		)
		Metrics.Errors.Add(1)
		return
	}
	accessLogger.Info("request served",
		zap.Int("http_code", http.StatusOK),
		zap.Duration("runtime_seconds", time.Since(t0)),
	)

	Metrics.Responses.Add(1)
}

func encodeFindResponse(format, query string, w http.ResponseWriter, metrics []pb3.GlobMatch) error {
	var err error
	var b []byte
	switch format {
	case formatTypeProtobuf, formatTypeProtobuf3:
		w.Header().Set("Content-Type", contentTypeProtobuf)
		var result pb3.GlobResponse
		result.Name = query
		result.Matches = metrics
		b, err = result.Marshal()
		/* #nosec */
		_, _ = w.Write(b)
	case formatTypeJSON:
		w.Header().Set("Content-Type", contentTypeJSON)
		jEnc := json.NewEncoder(w)
		err = jEnc.Encode(metrics)
	case formatTypeEmpty, formatTypePickle:
		w.Header().Set("Content-Type", contentTypePickle)

		var result []map[string]interface{}

		now := int32(time.Now().Unix() + 60)
		for _, metric := range metrics {
			// Tell graphite-web that we have everything
			var mm map[string]interface{}
			if config.GraphiteWeb09Compatibility {
				// graphite-web 0.9.x
				mm = map[string]interface{}{
					// graphite-web 0.9.x
					"metric_path": metric.Path,
					"isLeaf":      metric.IsLeaf,
				}
			} else {
				// graphite-web 1.0
				interval := &intervalset.IntervalSet{Start: 0, End: now}
				mm = map[string]interface{}{
					"is_leaf":   metric.IsLeaf,
					"path":      metric.Path,
					"intervals": interval,
				}
			}
			result = append(result, mm)
		}

		pEnc := pickle.NewEncoder(w)
		err = pEnc.Encode(result)
	}
	return err
}

func renderHandler(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	memoryUsage := 0
	uuid := uuid.NewV4()
	ctx := req.Context()

	ctx = util.SetUUID(ctx, uuid.String())
	logger := zapwriter.Logger("render").With(
		zap.Int("memory_usage_bytes", memoryUsage),
		zap.String("handler", "render"),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)

	logger.Debug("got render request",
		zap.String("request", req.URL.RequestURI()),
	)

	Metrics.Requests.Add(1)
	Metrics.RenderRequests.Add(1)

	accessLogger := zapwriter.Logger("access").With(
		zap.String("handler", "render"),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)

	err := req.ParseForm()
	if err != nil {
		http.Error(w, "failed to parse arguments", http.StatusBadRequest)
		accessLogger.Error("request failed",
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.String("reason", "failed to parse arguments"),
			zap.Int("http_code", http.StatusBadRequest),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	target := req.FormValue("target")
	format := req.FormValue("format")
	accessLogger = accessLogger.With(
		zap.String("format", format),
		zap.String("target", target),
	)

	from, err := strconv.Atoi(req.FormValue("from"))
	if err != nil {
		http.Error(w, "from is not a integer", http.StatusBadRequest)
		accessLogger.Error("request failed",
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.String("reason", "from is not a integer"),
			zap.Int("http_code", http.StatusBadRequest),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	until, err := strconv.Atoi(req.FormValue("until"))
	if err != nil {
		http.Error(w, "until is not a integer", http.StatusBadRequest)
		accessLogger.Error("request failed",
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.String("reason", "until is not a integer"),
			zap.Int("http_code", http.StatusBadRequest),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	if target == "" {
		http.Error(w, "empty target", http.StatusBadRequest)
		accessLogger.Error("request failed",
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.String("reason", "empty target"),
			zap.Int("http_code", http.StatusBadRequest),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	metrics, stats, err := config.zipper.Render(ctx, logger, target, int32(from), int32(until))
	sendStats(stats)
	if err != nil {
		http.Error(w, "error fetching the data", http.StatusInternalServerError)
		accessLogger.Error("request failed",
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.String("reason", err.Error()),
			zap.Int("http_code", http.StatusInternalServerError),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	var b []byte
	switch format {
	case formatTypeProtobuf, formatTypeProtobuf3:
		w.Header().Set("Content-Type", contentTypeProtobuf)
		b, err = metrics.Marshal()

		memoryUsage += len(b)
		/* #nosec */
		_, _ = w.Write(b)
	case formatTypeJSON:
		presponse := createRenderResponse(metrics, nil)
		w.Header().Set("Content-Type", contentTypeJSON)
		e := json.NewEncoder(w)
		err = e.Encode(presponse)
	case formatTypeEmpty, formatTypePickle:
		presponse := createRenderResponse(metrics, pickle.None{})
		w.Header().Set("Content-Type", contentTypePickle)
		e := pickle.NewEncoder(w)
		err = e.Encode(presponse)
	}

	if err != nil {
		http.Error(w, "error marshaling data", http.StatusInternalServerError)
		accessLogger.Error("render failed",
			zap.Int("http_code", http.StatusInternalServerError),
			zap.String("reason", "error marshaling data"),
			zap.Duration("runtime_seconds", time.Since(t0)),
			zap.Int("memory_usage_bytes", memoryUsage),
			zap.Error(err),
		)
		Metrics.Errors.Add(1)
		return
	}

	accessLogger.Info("request served",
		zap.Int("memory_usage_bytes", memoryUsage),
		zap.Int("http_code", http.StatusOK),
		zap.Duration("runtime_seconds", time.Since(t0)),
	)

	Metrics.Responses.Add(1)
}

func createRenderResponse(metrics *pb3.MultiFetchResponse, missing interface{}) []map[string]interface{} {

	var response []map[string]interface{}

	for _, metric := range metrics.GetMetrics() {

		var pvalues []interface{}
		for i, v := range metric.Values {
			if metric.IsAbsent[i] {
				pvalues = append(pvalues, missing)
			} else {
				pvalues = append(pvalues, v)
			}
		}

		// create the response
		presponse := map[string]interface{}{
			"start":  metric.StartTime,
			"step":   metric.StepTime,
			"end":    metric.StopTime,
			"name":   metric.Name,
			"values": pvalues,
		}
		response = append(response, presponse)
	}

	return response
}

func infoHandler(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	uuid := uuid.NewV4()
	ctx := req.Context()
	ctx = util.SetUUID(ctx, uuid.String())
	logger := zapwriter.Logger("info").With(
		zap.String("handler", "info"),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)

	logger.Debug("request",
		zap.String("request", req.URL.RequestURI()),
	)

	Metrics.Requests.Add(1)
	Metrics.InfoRequests.Add(1)

	accessLogger := zapwriter.Logger("access").With(
		zap.String("handler", "info"),
		zap.String("carbonzipper_uuid", uuid.String()),
		zap.String("carbonapi_uuid", util.GetUUID(ctx)),
	)
	err := req.ParseForm()
	if err != nil {
		http.Error(w, "failed to parse arguments", http.StatusBadRequest)
		accessLogger.Error("request failed",
			zap.String("reason", "failed to parse arguments"),
			zap.Int("http_code", http.StatusBadRequest),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		Metrics.Errors.Add(1)
		return
	}

	target := req.FormValue("target")
	format := req.FormValue("format")

	accessLogger = accessLogger.With(
		zap.String("target", target),
		zap.String("format", format),
	)

	if target == "" {
		accessLogger.Error("info failed",
			zap.Int("http_code", http.StatusBadRequest),
			zap.String("reason", "empty target"),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		http.Error(w, "info: empty target", http.StatusBadRequest)
		Metrics.Errors.Add(1)
		return
	}

	infos, stats, err := config.zipper.Info(ctx, logger, target)
	sendStats(stats)
	if err != nil {
		accessLogger.Error("info failed",
			zap.Int("http_code", http.StatusInternalServerError),
			zap.String("reason", err.Error()),
			zap.Duration("runtime_seconds", time.Since(t0)),
		)
		http.Error(w, "info: error processing request", http.StatusInternalServerError)
		Metrics.Errors.Add(1)
		return
	}

	var b []byte
	switch format {
	case formatTypeProtobuf, formatTypeProtobuf3:
		w.Header().Set("Content-Type", contentTypeProtobuf)
		var result pb3.ZipperInfoResponse
		result.Responses = make([]pb3.ServerInfoResponse, len(infos))
		for s, i := range infos {
			var r pb3.ServerInfoResponse
			r.Server = s
			r.Info = &i
			result.Responses = append(result.Responses, r)
		}
		b, err = result.Marshal()
		/* #nosec */
		_, _ = w.Write(b)
	case formatTypeEmpty, formatTypeJSON:
		w.Header().Set("Content-Type", contentTypeJSON)
		jEnc := json.NewEncoder(w)
		err = jEnc.Encode(infos)
	}
	if err != nil {
		http.Error(w, "error marshaling data", http.StatusInternalServerError)
		accessLogger.Error("info failed",
			zap.Int("http_code", http.StatusInternalServerError),
			zap.String("reason", "error marshaling data"),
			zap.Duration("runtime_seconds", time.Since(t0)),
			zap.Error(err),
		)
		Metrics.Errors.Add(1)
		return
	}
	accessLogger.Info("request served",
		zap.Int("http_code", http.StatusOK),
		zap.Duration("runtime_seconds", time.Since(t0)),
	)

	Metrics.Responses.Add(1)
}

func lbCheckHandler(w http.ResponseWriter, req *http.Request) {
	t0 := time.Now()
	logger := zapwriter.Logger("loadbalancer").With(zap.String("handler", "loadbalancer"))
	accessLogger := zapwriter.Logger("access").With(zap.String("handler", "loadbalancer"))
	logger.Debug("loadbalacner",
		zap.String("request", req.URL.RequestURI()),
	)

	Metrics.Requests.Add(1)

	/* #nosec */
	fmt.Fprintf(w, "Ok\n")
	accessLogger.Info("lb request served",
		zap.Int("http_code", http.StatusOK),
		zap.Duration("runtime_seconds", time.Since(t0)),
	)
	Metrics.Responses.Add(1)
}

func main() {
	err := zapwriter.ApplyConfig([]zapwriter.Config{defaultLoggerConfig})
	if err != nil {
		log.Fatal("Failed to initialize logger with default configuration")

	}
	logger := zapwriter.Logger("main")

	configFile := flag.String("config", "", "config file (yaml)")
	pidFile := flag.String("pid", "", "pidfile (default: empty, don't create pidfile)")

	flag.Parse()

	expvar.NewString("GoVersion").Set(runtime.Version())
	expvar.NewString("BuildVersion").Set(BuildVersion)

	if *configFile == "" {
		logger.Fatal("missing config file option")
	}

	cfg, err := ioutil.ReadFile(*configFile)
	if err != nil {
		logger.Fatal("unable to load config file:",
			zap.Error(err),
		)
	}

	err = yaml.Unmarshal(cfg, &config)
	if err != nil {
		logger.Fatal("failed to parse config",
			zap.String("config_path", *configFile),
			zap.Error(err),
		)
	}

	if len(config.Backends) == 0 {
		logger.Fatal("no Backends loaded -- exiting")
	}

	err = zapwriter.ApplyConfig(config.Logger)
	if err != nil {
		logger.Fatal("Failed to apply config",
			zap.Any("config", config.Logger),
			zap.Error(err),
		)
	}

	// Should print nicer stack traces in case of unexpected panic.
	defer func() {
		if r := recover(); r != nil {
			logger.Fatal("Recovered from unhandled panic",
				zap.Stack("stacktrace"),
			)
		}
	}()

	searchConfigured = len(config.CarbonSearch.Prefix) > 0 && len(config.CarbonSearch.Backend) > 0

	logger = zapwriter.Logger("main")
	logger.Info("starting carbonzipper",
		zap.String("build_version", BuildVersion),
		zap.Bool("carbonsearch_configured", searchConfigured),
		zap.Any("config", config),
	)

	runtime.GOMAXPROCS(config.MaxProcs)

	// +1 to track every over the number of buckets we track
	timeBuckets = make([]int64, config.Buckets+1)

	httputil.PublishTrackedConnections("httptrack")
	expvar.Publish("requestBuckets", expvar.Func(renderTimeBuckets))

	Metrics.Goroutines = expvar.Func(func() interface{} {
		return runtime.NumGoroutine()
	})
	expvar.Publish("goroutines", Metrics.Goroutines)

	// export config via expvars
	expvar.Publish("config", expvar.Func(func() interface{} { return config }))

	/* Configure zipper */
	// set up caches
	zipperConfig := &zipper.Config{
		PathCache:   pathcache.NewPathCache(config.ExpireDelaySec),
		SearchCache: pathcache.NewPathCache(config.ExpireDelaySec),

		ConcurrencyLimitPerServer: config.ConcurrencyLimitPerServer,
		MaxIdleConnsPerHost:       config.MaxIdleConnsPerHost,
		Backends:                  config.Backends,

		CarbonSearch:      config.CarbonSearch,
		Timeouts:          config.Timeouts,
		KeepAliveInterval: config.KeepAliveInterval,
	}

	Metrics.CacheSize = expvar.Func(func() interface{} { return zipperConfig.PathCache.ECSize() })
	expvar.Publish("cacheSize", Metrics.CacheSize)

	Metrics.CacheItems = expvar.Func(func() interface{} { return zipperConfig.PathCache.ECItems() })
	expvar.Publish("cacheItems", Metrics.CacheItems)

	Metrics.SearchCacheSize = expvar.Func(func() interface{} { return zipperConfig.SearchCache.ECSize() })
	expvar.Publish("searchCacheSize", Metrics.SearchCacheSize)

	Metrics.SearchCacheItems = expvar.Func(func() interface{} { return zipperConfig.SearchCache.ECItems() })
	expvar.Publish("searchCacheItems", Metrics.SearchCacheItems)

	config.zipper = zipper.NewZipper(sendStats, zipperConfig, zapwriter.Logger("zipper"))

	http.HandleFunc("/metrics/find/", httputil.TrackConnections(httputil.TimeHandler(util.ParseCtx(findHandler), bucketRequestTimes)))
	http.HandleFunc("/render/", httputil.TrackConnections(httputil.TimeHandler(util.ParseCtx(renderHandler), bucketRequestTimes)))
	http.HandleFunc("/info/", httputil.TrackConnections(httputil.TimeHandler(util.ParseCtx(infoHandler), bucketRequestTimes)))
	http.HandleFunc("/lb_check", lbCheckHandler)

	// nothing in the config? check the environment
	if config.Graphite.Host == "" {
		if host := os.Getenv("GRAPHITEHOST") + ":" + os.Getenv("GRAPHITEPORT"); host != ":" {
			config.Graphite.Host = host
		}
	}

	if config.Graphite.Pattern == "" {
		config.Graphite.Pattern = "{prefix}.{fqdn}"
	}

	if config.Graphite.Prefix == "" {
		config.Graphite.Prefix = "carbon.zipper"
	}

	// only register g2g if we have a graphite host
	if config.Graphite.Host != "" {
		// register our metrics with graphite
		graphite := g2g.NewGraphite(config.Graphite.Host, config.Graphite.Interval, 10*time.Second)

		/* #nosec */
		hostname, _ := os.Hostname()
		hostname = strings.Replace(hostname, ".", "_", -1)

		prefix := config.Graphite.Prefix

		pattern := config.Graphite.Pattern
		pattern = strings.Replace(pattern, "{prefix}", prefix, -1)
		pattern = strings.Replace(pattern, "{fqdn}", hostname, -1)

		graphite.Register(fmt.Sprintf("%s.requests", pattern), Metrics.Requests)
		graphite.Register(fmt.Sprintf("%s.responses", pattern), Metrics.Responses)
		graphite.Register(fmt.Sprintf("%s.errors", pattern), Metrics.Errors)

		graphite.Register(fmt.Sprintf("%s.find_requests", pattern), Metrics.FindRequests)
		graphite.Register(fmt.Sprintf("%s.find_errors", pattern), Metrics.FindErrors)

		graphite.Register(fmt.Sprintf("%s.render_requests", pattern), Metrics.RenderRequests)
		graphite.Register(fmt.Sprintf("%s.render_errors", pattern), Metrics.RenderErrors)

		graphite.Register(fmt.Sprintf("%s.info_requests", pattern), Metrics.InfoRequests)
		graphite.Register(fmt.Sprintf("%s.info_errors", pattern), Metrics.InfoErrors)

		graphite.Register(fmt.Sprintf("%s.timeouts", pattern), Metrics.Timeouts)

		for i := 0; i <= config.Buckets; i++ {
			var lower int
			if i == 0 {
				lower = 0
			} else {
				lower = 50 * (1 << (uint(i) - 1))
			}
			upper := 50 * (1 << uint(i))
			graphite.Register(fmt.Sprintf("%s.requests_in_%05dms_to_%05dms", pattern, lower, upper), bucketEntry(i))
		}

		graphite.Register(fmt.Sprintf("%s.cache_size", pattern), Metrics.CacheSize)
		graphite.Register(fmt.Sprintf("%s.cache_items", pattern), Metrics.CacheItems)

		graphite.Register(fmt.Sprintf("%s.search_cache_size", pattern), Metrics.SearchCacheSize)
		graphite.Register(fmt.Sprintf("%s.search_cache_items", pattern), Metrics.SearchCacheItems)

		graphite.Register(fmt.Sprintf("%s.cache_hits", pattern), Metrics.CacheHits)
		graphite.Register(fmt.Sprintf("%s.cache_misses", pattern), Metrics.CacheMisses)

		graphite.Register(fmt.Sprintf("%s.search_cache_hits", pattern), Metrics.SearchCacheHits)
		graphite.Register(fmt.Sprintf("%s.search_cache_misses", pattern), Metrics.SearchCacheMisses)

		go mstats.Start(config.Graphite.Interval)

		graphite.Register(fmt.Sprintf("%s.goroutines", pattern), Metrics.Goroutines)
		graphite.Register(fmt.Sprintf("%s.alloc", pattern), &mstats.Alloc)
		graphite.Register(fmt.Sprintf("%s.total_alloc", pattern), &mstats.TotalAlloc)
		graphite.Register(fmt.Sprintf("%s.num_gc", pattern), &mstats.NumGC)
		graphite.Register(fmt.Sprintf("%s.pause_ns", pattern), &mstats.PauseNS)
	}

	if *pidFile != "" {
		pidfile.SetPidfilePath(*pidFile)
		err = pidfile.Write()
		if err != nil {
			log.Fatalln("error during pidfile.Write():", err)
		}
	}

	err = gracehttp.Serve(&http.Server{
		Addr:    config.Listen,
		Handler: nil,
	})

	if err != nil {
		log.Fatal("error during gracehttp.Serve()",
			zap.Error(err),
		)
	}
}

var timeBuckets []int64

type bucketEntry int

func (b bucketEntry) String() string {
	return strconv.Itoa(int(atomic.LoadInt64(&timeBuckets[b])))
}

func renderTimeBuckets() interface{} {
	return timeBuckets
}

func bucketRequestTimes(req *http.Request, t time.Duration) {
	logger := zapwriter.Logger("slow")

	ms := t.Nanoseconds() / int64(time.Millisecond)

	// The buckets are delimited by the sequence:
	//	   0, 50, 100, 200, 400, 800, ...
	var bucket int
	for bucket = 0; bucket < config.Buckets+1; bucket++ {
		if ms >= 50*(1<<uint(bucket)) {
			bucket--
			break
		}
	}

	if bucket < config.Buckets {
		atomic.AddInt64(&timeBuckets[bucket], 1)
	} else {
		// Too big? Increment overflow bucket and log
		atomic.AddInt64(&timeBuckets[config.Buckets], 1)
		logger.Warn("Slow Request",
			zap.Duration("time", t),
			zap.String("url", req.URL.String()),
		)
	}
}

func sendStats(stats *zipper.Stats) {
	Metrics.Timeouts.Add(stats.Timeouts)
	Metrics.FindErrors.Add(stats.FindErrors)
	Metrics.RenderErrors.Add(stats.RenderErrors)
	Metrics.InfoErrors.Add(stats.InfoErrors)
	Metrics.SearchRequests.Add(stats.SearchRequests)
	Metrics.SearchCacheHits.Add(stats.SearchCacheHits)
	Metrics.SearchCacheMisses.Add(stats.SearchCacheMisses)
	Metrics.CacheMisses.Add(stats.CacheMisses)
	Metrics.CacheHits.Add(stats.CacheHits)
}
