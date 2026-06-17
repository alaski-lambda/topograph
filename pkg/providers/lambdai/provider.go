/*
 * Copyright 2026 NVIDIA CORPORATION
 * SPDX-License-Identifier: Apache-2.0
 */

package lambdai

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/NVIDIA/topograph/internal/httperr"
	"github.com/NVIDIA/topograph/internal/httpreq"
	"github.com/NVIDIA/topograph/pkg/providers"
	"github.com/NVIDIA/topograph/pkg/topology"
)

const (
	NAME = "lambdai"

	authWorkspaceID = "workspaceId"
	authToken       = "token"
	apiBaseURL      = "url"

	// apiPath is the Lambda topology API endpoint for listing instance topology.
	apiPath = "/api/v1/topology/instance"

	defaultPageSize = 200
)

type Client interface {
	WorkspaceID() string
	InstanceList(context.Context, *InstanceListRequest) (*InstanceListResponse, error)
	PageSize() int
}

type ClientFactory func(pageSize *int) (Client, error)

type baseProvider struct {
	clientFactory ClientFactory
	trimTiers     int
}

// lambdaiClient is a Topology API client.
type lambdaiClient struct {
	baseURL     string
	bearerToken string
	workspaceID string
	pageSize    int
}

// apiResponse is the envelope returned by the topology API: a "data" array of
// instances plus a pagination cursor ("page_token", null on the last page).
type apiResponse struct {
	Data      []InstanceTopology `json:"data"`
	PageToken string             `json:"page_token"`
}

// InstanceTopology represents the topology of a single instance.
type InstanceTopology struct {
	ID          string       `json:"id"`
	NetworkPath []NetworkHop `json:"networkPath"`
	NVLink      *NVLinkInfo  `json:"nvlink,omitempty"`
}

// NetworkHop is a single switch in an instance's network path, ordered from the
// leaf tier upward.
type NetworkHop struct {
	ID string `json:"id"`
}

type InstanceListRequest struct {
	Region    string
	PageSize  int
	PageToken string
}

type InstanceListResponse struct {
	Items         []InstanceTopology
	NextPageToken string
}

// NVLinkInfo represents NVLink domain information.
// NOTE: the populated shape is unverified — sampled staging instances all
// returned "nvlink": null. Revisit these tags once real NVLink data is available.
type NVLinkInfo struct {
	DomainID string `json:"domain_id,omitempty"`
	CliqueID string `json:"clique_id,omitempty"`
}

func (c *lambdaiClient) WorkspaceID() string {
	return c.workspaceID
}

func (c *lambdaiClient) PageSize() int {
	return c.pageSize
}

func (c *lambdaiClient) InstanceList(ctx context.Context, req *InstanceListRequest) (*InstanceListResponse, error) {
	headers := map[string]string{"Authorization": "Bearer " + c.bearerToken}
	query := map[string]string{
		"workspace_id": c.workspaceID,
		"region":       req.Region,
	}
	// page_size is a best-effort hint; the API paginates via page_token.
	if req.PageSize > 0 {
		query["page_size"] = strconv.Itoa(req.PageSize)
	}
	if req.PageToken != "" {
		query["page_token"] = req.PageToken
	}
	f := httpreq.GetRequestFunc(ctx, http.MethodGet, headers, query, nil, c.baseURL, apiPath)

	body, httpErr := httpreq.DoRequestWithRetries(f, false)
	if httpErr != nil {
		return nil, httpErr
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, httperr.NewError(http.StatusBadGateway, err.Error())
	}

	return &InstanceListResponse{
		Items:         apiResp.Data,
		NextPageToken: apiResp.PageToken,
	}, nil
}

func NamedLoader() (string, providers.Loader) {
	return NAME, Loader
}

func Loader(ctx context.Context, config providers.Config) (providers.Provider, *httperr.Error) {
	workspaceID, err := providers.StringFromMap(authWorkspaceID, config.Creds, true)
	if err != nil {
		return nil, httperr.NewError(http.StatusBadRequest, "credentials error: "+err.Error())
	}
	token, err := providers.StringFromMap(authToken, config.Creds, true)
	if err != nil {
		return nil, httperr.NewError(http.StatusBadRequest, "credentials error: "+err.Error())
	}
	baseURL, err := providers.StringFromMap(apiBaseURL, config.Params, true)
	if err != nil {
		return nil, httperr.NewError(http.StatusBadRequest, "parameters error: "+err.Error())
	}
	trimTiers, err := providers.GetTrimTiers(config.Params)
	if err != nil {
		return nil, httperr.NewError(http.StatusBadRequest, "parameters error: "+err.Error())
	}

	clientFactory := func(pageSize *int) (Client, error) {
		return &lambdaiClient{
			workspaceID: workspaceID,
			bearerToken: token,
			baseURL:     baseURL,
			pageSize:    getPageSize(pageSize),
		}, nil
	}

	return New(clientFactory, trimTiers), nil
}

func getPageSize(sz *int) int {
	if sz == nil {
		return defaultPageSize
	}
	return *sz
}

func (p *baseProvider) GenerateTopologyConfig(ctx context.Context, pageSize *int, instances []topology.ComputeInstances) (*topology.Graph, *httperr.Error) {
	topo, err := p.generateInstanceTopology(ctx, pageSize, instances)
	if err != nil {
		return nil, err
	}

	return topo.ToThreeTierGraph(NAME, instances, p.trimTiers, false), nil
}

type Provider struct {
	baseProvider
}

func New(clientFactory ClientFactory, trimTiers int) *Provider {
	return &Provider{
		baseProvider: baseProvider{
			clientFactory: clientFactory,
			trimTiers:     trimTiers,
		},
	}
}
