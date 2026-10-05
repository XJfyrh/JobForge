// Command supportapprovalproxy records actual action HTTP and pauses actual
// authorization/commit responses for the separately released S4 experiment.
// It is excluded from every production image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type fault struct {
	RunID       string `json:"run_id"`
	TenantID    string `json:"tenant_id"`
	ProfileHash string `json:"profile_hash"`
	Boundary    string `json:"boundary"`
}

type experiment struct {
	directory string
	mu        sync.Mutex
	used      map[string]bool
}

func (e *experiment) fault() (fault, error) {
	var f fault
	raw, err := os.ReadFile(filepath.Join(e.directory, "fault.json"))
	if err != nil {
		return f, err
	}
	if len(raw) > 2048 || jsonstrict.Decode(raw, &f) != nil || !run.ValidUUID(f.RunID) || !run.ValidIdentifier(f.TenantID) || !run.ValidHash(f.ProfileHash) ||
		(f.Boundary != "authorization_saved" && f.Boundary != "business_committed") {
		return f, errors.New("FAULT_INVALID")
	}
	return f, nil
}

func (e *experiment) publish(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(e.directory, ".event-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Link(file.Name(), filepath.Join(e.directory, name))
}

func (e *experiment) selectFault(f fault) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.used[f.RunID] {
		return false
	}
	e.used[f.RunID] = true
	return true
}

func (e *experiment) hold(ctx context.Context, f fault, value any) error {
	name := f.RunID + ".active"
	if err := e.publish(name, map[string]any{"active": true}); err != nil {
		return err
	}
	defer func() { _ = os.Remove(filepath.Join(e.directory, name)) }()
	if err := e.publish(f.RunID+".barrier.json", value); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if f.Boundary == "authorization_saved" {
				if _, err := os.Stat(filepath.Join(e.directory, f.RunID+".release")); err == nil {
					return nil
				}
			}
		}
	}
}

func (e *experiment) grpcForward(connection *grpc.ClientConn) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
		name := strings.TrimPrefix(info.FullMethod, "/jobforge.agent.v1.AgentService/")
		method := agentv1.File_jobforge_agent_v1_agent_proto.Services().ByName("AgentService").Methods().ByName(protoreflect.Name(name))
		values := metadata.ValueFromIncomingContext(ctx, "authorization")
		if method == nil || len(values) != 1 {
			return nil, status.Error(codes.Unauthenticated, "RPC_UNAVAILABLE")
		}
		typeOfResponse, err := protoregistry.GlobalTypes.FindMessageByName(method.Output().FullName())
		if err != nil {
			return nil, status.Error(codes.Internal, "RPC_UNAVAILABLE")
		}
		f, faultErr := e.fault()
		if faultErr != nil && !errors.Is(faultErr, os.ErrNotExist) {
			return nil, status.Error(codes.Unavailable, "FAULT_INVALID")
		}
		bounded, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", values[0])), 5*time.Second)
		defer cancel()
		response := typeOfResponse.New().Interface()
		if err := connection.Invoke(bounded, info.FullMethod, request, response); err != nil {
			return nil, err
		}
		if req, ok := request.(*agentv1.AuthorizeActionRequest); ok && faultErr == nil && f.Boundary == "authorization_saved" && req.Execution != nil && req.Step != nil &&
			req.Execution.RunId == f.RunID && req.Execution.TenantId == f.TenantID && req.Step.ProfileHash == f.ProfileHash && e.selectFault(f) {
			if err := e.hold(ctx, f, map[string]any{"schema_version": 1, "boundary": f.Boundary, "execution": req.Execution, "step": req.Step, "observed_at": time.Now().UTC()}); err != nil {
				return nil, status.Error(codes.Unavailable, "ACK_WITHHELD")
			}
		}
		return response, nil
	}
}

func (e *experiment) httpForward(origin string) http.Handler {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 8 * time.Second}, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.RawQuery != "" || (req.Method != http.MethodGet && req.Method != http.MethodPost) || !strings.HasPrefix(req.URL.Path, "/business/v1/actions/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(req.Body, business.ActionRequestMaxBytes+1))
		if err != nil || len(raw) > business.ActionRequestMaxBytes {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var action business.SignedAction
		operation := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/business/v1/actions/"), "/receipt")
		if req.Method == http.MethodPost {
			action, err = business.DecodeSignedAction(raw)
			if err != nil || req.URL.Path != "/business/v1/actions/apply_ticket_resolution" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			operation = action.Authorization.OperationID
		} else if !run.ValidUUID(operation) || req.URL.Path != "/business/v1/actions/"+operation+"/receipt" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, faultErr := e.fault()
		if faultErr != nil && !errors.Is(faultErr, os.ErrNotExist) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		id := fmt.Sprintf("http-%d", time.Now().UnixNano())
		if e.publish(id+".dispatch.json", map[string]any{"schema_version": 1, "operation_id": operation, "method": req.Method, "observed_at": time.Now().UTC()}) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		upstream, err := http.NewRequestWithContext(req.Context(), req.Method, origin+req.URL.Path, strings.NewReader(string(raw)))
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		upstream.Header.Set("Authorization", req.Header.Get("Authorization"))
		upstream.Header.Set("Content-Type", "application/json")
		response, err := client.Do(upstream)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
		if err != nil || len(body) > 4096 || e.publish(id+".response.json", map[string]any{"schema_version": 1, "operation_id": operation, "method": req.Method, "http_status": response.StatusCode, "body": json.RawMessage(body), "observed_at": time.Now().UTC()}) != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if req.Method == http.MethodPost && response.StatusCode == 200 && faultErr == nil && f.Boundary == "business_committed" && action.Authorization.RunID == f.RunID && action.Authorization.TenantID == f.TenantID && e.selectFault(f) {
			var receipt business.ActionReceipt
			if jsonstrict.Decode(body, &receipt) != nil || receipt.Validate(action) != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = e.hold(req.Context(), f, map[string]any{"schema_version": 1, "boundary": f.Boundary, "run_id": f.RunID, "operation_id": operation, "receipt": json.RawMessage(body), "observed_at": time.Now().UTC()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	})
}

func main() {
	upstream := flag.String("upstream", "control:8094", "fixed control gateway")
	origin := flag.String("business", "http://business:8092", "actual business server")
	directory := flag.String("directory", "/var/lib/jobforge/approval", "private experiment events")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	connection, err := grpc.NewClient(*upstream, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(grpcapi.MaxMessageBytes), grpc.MaxCallSendMsgSize(grpcapi.MaxMessageBytes)))
	if err != nil {
		os.Exit(1)
	}
	defer func() { _ = connection.Close() }()
	e := &experiment{directory: *directory, used: map[string]bool{}}
	server := grpc.NewServer(grpc.MaxRecvMsgSize(grpcapi.MaxMessageBytes), grpc.MaxSendMsgSize(grpcapi.MaxMessageBytes), grpc.UnaryInterceptor(e.grpcForward(connection)))
	agentv1.RegisterAgentServiceServer(server, &agentv1.UnimplementedAgentServiceServer{})
	listener, err := net.Listen("tcp", "127.0.0.1:8095")
	if err != nil {
		os.Exit(1)
	}
	httpServer := &http.Server{Addr: "0.0.0.0:8096", Handler: e.httpForward(*origin), ReadHeaderTimeout: time.Second}
	httpListener, err := net.Listen("tcp", httpServer.Addr)
	if err != nil {
		os.Exit(1)
	}
	go func() {
		if err := httpServer.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cancel()
		}
	}()
	go func() { <-ctx.Done(); server.Stop(); _ = httpServer.Close() }()
	if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		os.Exit(1)
	}
}
