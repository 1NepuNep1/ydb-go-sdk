package ydb

import (
	"context"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Discovery_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Discovery"
	"google.golang.org/protobuf/proto"
)

type Endpoint struct {
	Addr       string
	Port       int
	LoadFactor float32
	Local      bool
}

type discoveryClient struct {
	discoveryService Ydb_Discovery_V1.DiscoveryServiceClient
	database         string
	ssl              bool
}

func (d *discoveryClient) Discover(ctx context.Context) ([]Endpoint, error) {
	request := Ydb_Discovery.ListEndpointsRequest{
		Database: d.database,
	}
	response, err := d.discoveryService.ListEndpoints(ctx, &request)
	if err != nil {
		return nil, err
	}
	listEndpointsResult := Ydb_Discovery.ListEndpointsResult{}
	err = proto.Unmarshal(response.GetOperation().GetResult().GetValue(), &listEndpointsResult)
	if err != nil {
		return nil, err
	}
	endpoints := make([]Endpoint, 0, len(listEndpointsResult.Endpoints))
	for _, e := range listEndpointsResult.Endpoints {
		if e.Ssl == d.ssl {
			endpoints = append(endpoints, Endpoint{
				Addr:  e.Address,
				Port:  int(e.Port),
				Local: e.Location == listEndpointsResult.SelfLocation,
			})
		}
	}
	return endpoints, nil
}
