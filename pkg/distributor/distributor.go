package distributor

import (
	"context"
	"flag"
	"hash/fnv"
	"sync/atomic"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/weaveworks/common/instrument"
	"github.com/weaveworks/common/user"
	cortex_client "github.com/weaveworks/cortex/pkg/ingester/client"
	"github.com/weaveworks/cortex/pkg/ring"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/grafana/tempo/pkg/ingester/client"
	"github.com/grafana/tempo/pkg/logproto"
)

var (
	sendDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "tempo",
		Name:      "distributor_send_duration_seconds",
		Help:      "Time spent sending a sample batch to multiple replicated ingesters.",
		Buckets:   []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"method", "status_code"})
	ingesterAppends = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_appends_total",
		Help:      "The total number of batch appends sent to ingesters.",
	}, []string{"ingester"})
	ingesterAppendFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempo",
		Name:      "distributor_ingester_append_failures_total",
		Help:      "The total number of failed batch appends sent to ingesters.",
	}, []string{"ingester"})
)

func init() {
	prometheus.MustRegister(sendDuration)
	prometheus.MustRegister(ingesterAppends)
	prometheus.MustRegister(ingesterAppendFailures)
}

// Config for a Distributor.
type Config struct {
	RemoteTimeout time.Duration
	ClientConfig  client.Config
	PoolConfig    cortex_client.PoolConfig
}

func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.ClientConfig.RegisterFlags(f)
	cfg.PoolConfig.RegisterFlags(f)

	f.DurationVar(&cfg.RemoteTimeout, "ingester.remote-timeout", 10*time.Second, "")
}

// Distributor coordinates replicates and distribution of log streams.
type Distributor struct {
	cfg  Config
	ring ring.ReadRing
	pool *cortex_client.Pool
}

// New a distributor creates.
func New(cfg Config, ring ring.ReadRing) (*Distributor, error) {
	factory := func(addr string) (grpc_health_v1.HealthClient, error) {
		return client.New(cfg.ClientConfig, addr)
	}
	cfg.PoolConfig.RemoteTimeout = cfg.RemoteTimeout

	return &Distributor{
		cfg:  cfg,
		ring: ring,
		pool: cortex_client.NewPool(cfg.PoolConfig, ring, factory),
	}, nil
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type streamTracker struct {
	stream      *logproto.Stream
	minSuccess  int
	maxFailures int
	succeeded   int32
	failed      int32
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
type pushTracker struct {
	samplesPending int32
	samplesFailed  int32
	done           chan struct{}
	err            chan error
}

// Push a set of streams.
func (d *Distributor) Push(ctx context.Context, req *logproto.PushRequest) (*logproto.PushResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}

	// First we flatten out the request into a list of samples.
	// We use the heuristic of 1 sample per TS to size the array.
	// We also work out the hash value at the same time.
	streams := make([]streamTracker, len(req.Streams), len(req.Streams))
	keys := make([]uint32, 0, len(req.Streams))
	for i, stream := range req.Streams {
		keys = append(keys, tokenFor(userID, stream.Labels))
		streams[i].stream = stream
	}

	if len(streams) == 0 {
		return &logproto.PushResponse{}, nil
	}

	replicationSets, err := d.ring.BatchGet(keys, ring.Write)
	if err != nil {
		return nil, err
	}

	samplesByIngester := map[*ring.IngesterDesc][]*streamTracker{}
	for i, replicationSet := range replicationSets {
		streams[i].minSuccess = len(replicationSet.Ingesters) - replicationSet.MaxErrors
		streams[i].maxFailures = replicationSet.MaxErrors
		for _, ingester := range replicationSet.Ingesters {
			samplesByIngester[ingester] = append(samplesByIngester[ingester], &streams[i])
		}
	}

	pushTracker := pushTracker{
		samplesPending: int32(len(streams)),
		done:           make(chan struct{}),
		err:            make(chan error),
	}
	for ingester, samples := range samplesByIngester {
		go func(ingester *ring.IngesterDesc, samples []*streamTracker) {
			// Use a background context to make sure all ingesters get samples even if we return early
			localCtx, cancel := context.WithTimeout(context.Background(), d.cfg.RemoteTimeout)
			defer cancel()
			localCtx = user.InjectOrgID(localCtx, userID)
			if sp := opentracing.SpanFromContext(ctx); sp != nil {
				localCtx = opentracing.ContextWithSpan(localCtx, sp)
			}
			d.sendSamples(localCtx, ingester, samples, &pushTracker)
		}(ingester, samples)
	}
	select {
	case err := <-pushTracker.err:
		return nil, err
	case <-pushTracker.done:
		return &logproto.PushResponse{}, nil
	}
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamples(ctx context.Context, ingester *ring.IngesterDesc, streamTrackers []*streamTracker, pushTracker *pushTracker) {
	err := d.sendSamplesErr(ctx, ingester, streamTrackers)

	// If we succeed, decrement each sample's pending count by one.  If we reach
	// the required number of successful puts on this sample, then decrement the
	// number of pending samples by one.  If we successfully push all samples to
	// min success ingesters, wake up the waiting rpc so it can return early.
	// Similarly, track the number of errors, and if it exceeds maxFailures
	// shortcut the waiting rpc.
	//
	// The use of atomic increments here guarantees only a single sendSamples
	// goroutine will write to either channel.
	for i := range streamTrackers {
		if err != nil {
			if atomic.AddInt32(&streamTrackers[i].failed, 1) <= int32(streamTrackers[i].maxFailures) {
				continue
			}
			if atomic.AddInt32(&pushTracker.samplesFailed, 1) == 1 {
				pushTracker.err <- err
			}
		} else {
			if atomic.AddInt32(&streamTrackers[i].succeeded, 1) != int32(streamTrackers[i].minSuccess) {
				continue
			}
			if atomic.AddInt32(&pushTracker.samplesPending, -1) == 0 {
				pushTracker.done <- struct{}{}
			}
		}
	}
}

// TODO taken from Cortex, see if we can refactor out an usable interface.
func (d *Distributor) sendSamplesErr(ctx context.Context, ingester *ring.IngesterDesc, streams []*streamTracker) error {
	c, err := d.pool.GetClientFor(ingester.Addr)
	if err != nil {
		return err
	}

	req := &logproto.PushRequest{
		Streams: make([]*logproto.Stream, len(streams)),
	}
	for i, s := range streams {
		req.Streams[i] = s.stream
	}

	err = instrument.TimeRequestHistogram(ctx, "Distributor.sendSamples", sendDuration, func(ctx context.Context) error {
		_, err := c.(logproto.PusherClient).Push(ctx, req)
		return err
	})
	ingesterAppends.WithLabelValues(ingester.Addr).Inc()
	if err != nil {
		ingesterAppendFailures.WithLabelValues(ingester.Addr).Inc()
	}
	return err
}

func tokenFor(userID, labels string) uint32 {
	h := fnv.New32()
	h.Write([]byte(userID))
	h.Write([]byte(labels))
	return h.Sum32()
}

// Check implements the grpc healthcheck
func (*Distributor) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}
