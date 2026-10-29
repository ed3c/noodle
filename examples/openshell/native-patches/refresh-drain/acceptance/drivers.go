//go:build unix

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Official v0.1.5 descriptors; no replacement gateway or hand-written wire codec.
//
//go:embed contracts.pb
var contracts []byte

type gate struct {
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	openOnce sync.Once
}

func newGate() *gate  { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }
func (g *gate) open() { g.openOnce.Do(func() { close(g.release) }) }
func (g *gate) wait(ctx context.Context) error {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type driverFixture struct {
	mu               sync.Mutex
	directory        string
	values           map[string]string
	storeGate        *gate
	stores           int
	computeMutations int
}

func newDriverFixture(dir string) *driverFixture {
	return &driverFixture{directory: dir, values: map[string]string{}, storeGate: newGate()}
}

func textField(m protoreflect.Message, key string) string {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(key))).String()
}

func metadata(family string) map[string]any {
	return map[string]any{"protocol_version": map[string]any{"major": 1}, "implementation_name": "noodle/shutdown-fixture", "implementation_version": "1", "supported_capabilities": []string{"openshell." + family + ".contract"}}
}

func (f *driverFixture) invoke(ctx context.Context, service, method string, m protoreflect.Message) (map[string]any, error) {
	if service == "openshell.compute.v1.ComputeDriver" {
		switch method {
		case "GetCapabilities":
			return map[string]any{"driver_name": "fixture", "extension": metadata("compute"), "resource_admission_policy": `v1:{"allow_driver_config":false,"resource_admission":{"enabled":true,"required_labels":{"openshell.ai/sandbox-attachable":"true","openshell.ai/sandbox-attachable-workspace":"${workspace}"}}}`}, nil
		case "ListSandboxes", "EnsureWorkspace":
			return map[string]any{}, nil
		default:
			f.mu.Lock()
			f.computeMutations++
			f.mu.Unlock()
			return nil, status.Error(codes.FailedPrecondition, "fixture forbids compute effects")
		}
	}
	switch method {
	case "GetCapabilities":
		return map[string]any{"driver_name": "fixture", "backend_kind": "acceptance-only", "extension": metadata("credentials")}, nil
	case "StoreCredential":
		value := textField(m, "value")
		if value == "fake-replacement" {
			if err := f.storeGate.wait(ctx); err != nil {
				return nil, status.Error(codes.Canceled, "fixture store gate canceled")
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stores++
		handle := fmt.Sprintf("fixture-%d", f.stores)
		f.values[handle] = value
		if err := f.persist(); err != nil {
			return nil, status.Error(codes.Internal, "fixture persistence failed")
		}
		return map[string]any{"handle": map[string]any{"driver": "fixture", "handle": handle}}, nil
	case "ResolveCredentials":
		f.mu.Lock()
		defer f.mu.Unlock()
		var values []any
		list := m.Get(m.Descriptor().Fields().ByName("credentials")).List()
		for i := 0; i < list.Len(); i++ {
			r := list.Get(i).Message()
			h := r.Get(r.Descriptor().Fields().ByName("handle")).Message()
			value, ok := f.values[textField(h, "handle")]
			if !ok {
				return nil, status.Error(codes.NotFound, "fixture handle not found")
			}
			values = append(values, map[string]any{"request_id": textField(r, "request_id"), "value": value})
		}
		return map[string]any{"credentials": values}, nil
	case "DeleteCredential":
		f.mu.Lock()
		defer f.mu.Unlock()
		h := m.Get(m.Descriptor().Fields().ByName("handle")).Message()
		delete(f.values, textField(h, "handle"))
		return map[string]any{}, f.persist()
	default:
		return nil, status.Error(codes.Unimplemented, "fixture method unsupported")
	}
}

// The fake driver acknowledges only after its isolated storage has been synced.
// Its contents are synthetic test values; never populated from the environment.
func (f *driverFixture) persist() error {
	b, err := json.Marshal(f.values)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(f.directory, "driver-values.json"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = file.Write(b); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	dir, err := os.Open(f.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (f *driverFixture) reloadPersisted() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(f.directory, "driver-values.json"))
	if err != nil {
		return err
	}
	var persisted map[string]string
	if err := json.Unmarshal(b, &persisted); err != nil {
		return err
	}
	found := false
	for _, value := range persisted {
		found = found || value == "fake-replacement"
	}
	if !found {
		return fmt.Errorf("replacement missing from persisted fixture storage")
	}
	f.values = persisted
	return nil
}

func (f *driverFixture) serve(socket, service string) (func(), error) {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(contracts, &set); err != nil {
		return nil, err
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, err
	}
	d, err := files.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, err
	}
	sd := d.(protoreflect.ServiceDescriptor)
	server := grpc.NewServer()
	desc := grpc.ServiceDesc{ServiceName: service, HandlerType: (*interface{})(nil)}
	for i := 0; i < sd.Methods().Len(); i++ {
		method := sd.Methods().Get(i)
		if method.IsStreamingServer() {
			desc.Streams = append(desc.Streams, grpc.StreamDesc{StreamName: string(method.Name()), ServerStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				if err := stream.RecvMsg(dynamicpb.NewMessage(method.Input())); err != nil {
					return err
				}
				<-stream.Context().Done()
				return nil
			}})
			continue
		}
		desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: string(method.Name()), Handler: func(_ any, ctx context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			request := dynamicpb.NewMessage(method.Input())
			if err := decode(request); err != nil {
				return nil, err
			}
			body, err := f.invoke(ctx, service, string(method.Name()), request)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			response := dynamicpb.NewMessage(method.Output())
			if err := protojson.Unmarshal(encoded, response); err != nil {
				return nil, err
			}
			return response, nil
		}})
	}
	server.RegisterService(&desc, f)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	go server.Serve(l)
	return func() { server.Stop(); _ = l.Close() }, nil
}
