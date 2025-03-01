package grpc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/dynamic/grpcdynamic"
	"github.com/jhump/protoreflect/grpcreflect"
	ammo "github.com/yandex/pandora/components/providers/grpc"
	"github.com/yandex/pandora/core"
	"github.com/yandex/pandora/core/aggregator/netsample"
	"github.com/yandex/pandora/core/warmup"
	"github.com/yandex/pandora/lib/answlog"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const defaultTimeout = time.Second * 15

type Sample struct {
	URL              string
	ShootTimeSeconds float64
}

type grpcDialOptions struct {
	Authority string        `config:"authority"`
	Timeout   time.Duration `config:"timeout"`
}

type GunConfig struct {
	Target      string          `validate:"required"`
	Timeout     time.Duration   `config:"timeout"` // grpc request timeout
	TLS         bool            `config:"tls"`
	DialOptions grpcDialOptions `config:"dial_options"`
	AnswLog     AnswLogConfig   `config:"answlog"`
}

type AnswLogConfig struct {
	Enabled bool   `config:"enabled"`
	Path    string `config:"path"`
	Filter  string `config:"filter" valid:"oneof=all warning error"`
}

type Gun struct {
	DebugLog bool
	client   *grpc.ClientConn
	conf     GunConfig
	aggr     core.Aggregator
	core.GunDeps

	stub     grpcdynamic.Stub
	services map[string]desc.MethodDescriptor

	answLog *zap.Logger
}

func DefaultGunConfig() GunConfig {
	return GunConfig{
		Target: "default target",
		AnswLog: AnswLogConfig{
			Enabled: false,
			Path:    "answ.log",
			Filter:  "all",
		},
	}
}

func (g *Gun) WarmUp(opts *warmup.Options) (interface{}, error) {
	conn, err := makeGRPCConnect(g.conf.Target, g.conf.TLS, g.conf.DialOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to target: %w", err)
	}
	defer conn.Close()

	meta := make(metadata.MD)
	refCtx := metadata.NewOutgoingContext(context.Background(), meta)
	refClient := grpcreflect.NewClientAuto(refCtx, conn)
	listServices, err := refClient.ListServices()
	if err != nil {
		opts.Log.Error("failed to get services list", zap.Error(err)) // WarmUp calls before Bind()
		return nil, fmt.Errorf("refClient.ListServices err: %w", err)
	}
	services := make(map[string]desc.MethodDescriptor)
	for _, s := range listServices {
		service, err := refClient.ResolveService(s)
		if err != nil {
			if grpcreflect.IsElementNotFoundError(err) {
				continue
			}
			opts.Log.Error("cant resolveService", zap.String("service_name", s), zap.Error(err)) // WarmUp calls before Bind()
			return nil, fmt.Errorf("cant resolveService %s; err: %w", s, err)
		}
		listMethods := service.GetMethods()
		for _, m := range listMethods {
			services[m.GetFullyQualifiedName()] = *m
		}
	}
	return services, nil
}

func (g *Gun) AcceptWarmUpResult(i interface{}) error {
	services, ok := i.(map[string]desc.MethodDescriptor)
	if !ok {
		return fmt.Errorf("grpc WarmUp result should be services: map[string]desc.MethodDescriptor")
	}
	g.services = services
	return nil
}

func NewGun(conf GunConfig) *Gun {
	answLog := answlog.Init(conf.AnswLog.Path)
	return &Gun{conf: conf, answLog: answLog}
}

func (g *Gun) Bind(aggr core.Aggregator, deps core.GunDeps) error {
	conn, err := makeGRPCConnect(g.conf.Target, g.conf.TLS, g.conf.DialOptions)
	if err != nil {
		return fmt.Errorf("makeGRPCConnect fail %w", err)
	}
	g.client = conn
	g.aggr = aggr
	g.GunDeps = deps
	g.stub = grpcdynamic.NewStub(conn)

	if ent := deps.Log.Check(zap.DebugLevel, "Gun bind"); ent != nil {
		deps.Log.Warn("Deprecation Warning: log level: debug doesn't produce request/response logs anymore. Please use AnswLog option instead:\nanswlog:\n  enabled: true\n  filter: all|warning|error\n  path: answ.log")
		g.DebugLog = true
	}

	return nil
}

func (g *Gun) Shoot(am core.Ammo) {
	customAmmo := am.(*ammo.Ammo)
	g.shoot(customAmmo)
}

func (g *Gun) shoot(ammo *ammo.Ammo) {
	code := 0
	sample := netsample.Acquire(ammo.Tag)
	defer func() {
		sample.SetProtoCode(code)
		g.aggr.Report(sample)
	}()

	method, ok := g.services[ammo.Call]
	if !ok {
		g.GunDeps.Log.Error("invalid ammo.Call", zap.String("method", ammo.Call),
			zap.Strings("allowed_methods", maps.Keys(g.services)))
		return
	}

	payloadJSON, err := json.Marshal(ammo.Payload)
	if err != nil {
		g.GunDeps.Log.Error("invalid payload. Cant unmarshal json", zap.Error(err))
		return
	}
	md := method.GetInputType()
	message := dynamic.NewMessage(md)
	err = message.UnmarshalJSON(payloadJSON)
	if err != nil {
		code = 400
		g.GunDeps.Log.Error("invalid payload. Cant unmarshal gRPC", zap.Error(err))
		return
	}

	timeout := defaultTimeout
	if g.conf.Timeout != 0 {
		timeout = g.conf.Timeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.New(ammo.Metadata))
	out, grpcErr := g.stub.InvokeRpc(ctx, &method, message)
	code = convertGrpcStatus(grpcErr)

	if grpcErr != nil {
		g.GunDeps.Log.Error("response error", zap.Error(err))
	}

	if g.conf.AnswLog.Enabled {
		switch g.conf.AnswLog.Filter {
		case "all":
			g.answLogging(g.answLog, &method, message, out, grpcErr)

		case "warning":
			if code >= 400 {
				g.answLogging(g.answLog, &method, message, out, grpcErr)
			}

		case "error":
			if code >= 500 {
				g.answLogging(g.answLog, &method, message, out, grpcErr)
			}
		}
	}
}

func (g *Gun) answLogging(logger *zap.Logger, method *desc.MethodDescriptor, request proto.Message, response proto.Message, grpcErr error) {
	logger.Debug("Request:", zap.Stringer("method", method), zap.Stringer("message", request))
	logger.Debug("Response:", zap.Stringer("resp", response), zap.Error(grpcErr))
}

func makeGRPCConnect(target string, isTLS bool, dialOptions grpcDialOptions) (conn *grpc.ClientConn, err error) {
	opts := []grpc.DialOption{}
	if isTLS {
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	timeout := time.Second
	if dialOptions.Timeout != 0 {
		timeout = dialOptions.Timeout
	}
	opts = append(opts, grpc.WithUserAgent("load test, pandora universal grpc shooter"))

	if dialOptions.Authority != "" {
		opts = append(opts, grpc.WithAuthority(dialOptions.Authority))
	}

	ctx, cncl := context.WithTimeout(context.Background(), timeout)
	defer cncl()
	return grpc.DialContext(ctx, target, opts...)
}

func convertGrpcStatus(err error) int {
	s := status.Convert(err)

	switch s.Code() {
	case codes.OK:
		return 200
	case codes.Canceled:
		return 499
	case codes.InvalidArgument:
		return 400
	case codes.DeadlineExceeded:
		return 504
	case codes.NotFound:
		return 404
	case codes.AlreadyExists:
		return 409
	case codes.PermissionDenied:
		return 403
	case codes.ResourceExhausted:
		return 429
	case codes.FailedPrecondition:
		return 400
	case codes.Aborted:
		return 409
	case codes.OutOfRange:
		return 400
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	case codes.Unauthenticated:
		return 401
	default:
		return 500
	}
}

var _ warmup.WarmedUp = (*Gun)(nil)
