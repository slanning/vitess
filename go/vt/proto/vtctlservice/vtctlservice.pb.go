// Code generated by protoc-gen-go.
// source: vtctlservice.proto
// DO NOT EDIT!

/*
Package vtctlservice is a generated protocol buffer package.

It is generated from these files:
	vtctlservice.proto

It has these top-level messages:
*/
package vtctlservice

import proto "github.com/golang/protobuf/proto"
import vtctldata "github.com/youtube/vitess/go/vt/proto/vtctldata"

import (
	context "golang.org/x/net/context"
	grpc "google.golang.org/grpc"
)

// Reference imports to suppress errors if they are not otherwise used.
var _ context.Context
var _ grpc.ClientConn

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal

// Client API for Vtctl service

type VtctlClient interface {
	ExecuteVtctlCommand(ctx context.Context, in *vtctldata.ExecuteVtctlCommandRequest, opts ...grpc.CallOption) (Vtctl_ExecuteVtctlCommandClient, error)
}

type vtctlClient struct {
	cc *grpc.ClientConn
}

func NewVtctlClient(cc *grpc.ClientConn) VtctlClient {
	return &vtctlClient{cc}
}

func (c *vtctlClient) ExecuteVtctlCommand(ctx context.Context, in *vtctldata.ExecuteVtctlCommandRequest, opts ...grpc.CallOption) (Vtctl_ExecuteVtctlCommandClient, error) {
	stream, err := grpc.NewClientStream(ctx, &_Vtctl_serviceDesc.Streams[0], c.cc, "/vtctlservice.Vtctl/ExecuteVtctlCommand", opts...)
	if err != nil {
		return nil, err
	}
	x := &vtctlExecuteVtctlCommandClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type Vtctl_ExecuteVtctlCommandClient interface {
	Recv() (*vtctldata.ExecuteVtctlCommandResponse, error)
	grpc.ClientStream
}

type vtctlExecuteVtctlCommandClient struct {
	grpc.ClientStream
}

func (x *vtctlExecuteVtctlCommandClient) Recv() (*vtctldata.ExecuteVtctlCommandResponse, error) {
	m := new(vtctldata.ExecuteVtctlCommandResponse)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Server API for Vtctl service

type VtctlServer interface {
	ExecuteVtctlCommand(*vtctldata.ExecuteVtctlCommandRequest, Vtctl_ExecuteVtctlCommandServer) error
}

func RegisterVtctlServer(s *grpc.Server, srv VtctlServer) {
	s.RegisterService(&_Vtctl_serviceDesc, srv)
}

func _Vtctl_ExecuteVtctlCommand_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(vtctldata.ExecuteVtctlCommandRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(VtctlServer).ExecuteVtctlCommand(m, &vtctlExecuteVtctlCommandServer{stream})
}

type Vtctl_ExecuteVtctlCommandServer interface {
	Send(*vtctldata.ExecuteVtctlCommandResponse) error
	grpc.ServerStream
}

type vtctlExecuteVtctlCommandServer struct {
	grpc.ServerStream
}

func (x *vtctlExecuteVtctlCommandServer) Send(m *vtctldata.ExecuteVtctlCommandResponse) error {
	return x.ServerStream.SendMsg(m)
}

var _Vtctl_serviceDesc = grpc.ServiceDesc{
	ServiceName: "vtctlservice.Vtctl",
	HandlerType: (*VtctlServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "ExecuteVtctlCommand",
			Handler:       _Vtctl_ExecuteVtctlCommand_Handler,
			ServerStreams: true,
		},
	},
}
