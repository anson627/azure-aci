package provider

import (
	"context"

	azaci "github.com/Azure/azure-sdk-for-go/services/containerinstance/mgmt/2021-10-01/containerinstance"
	"github.com/virtual-kubelet/azure-aci/pkg/client"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
)

type CreateContainerGroupFunc func(ctx context.Context, resourceGroup, podNS, podName string, cg *client.ContainerGroupWrapper) error
type GetContainerGroupInfoFunc func(ctx context.Context, resourceGroup, namespace, name, nodeName string) (*azaci.ContainerGroup, error)
type GetContainerGroupListFunc func(ctx context.Context, resourceGroup string) (*[]azaci.ContainerGroup, error)
type ListCapabilitiesFunc func(ctx context.Context, region string) (*[]azaci.Capabilities, error)
type DeleteContainerGroupFunc func(ctx context.Context, resourceGroup, cgName string) error
type ListLogsFunc func(ctx context.Context, resourceGroup, cgName, containerName string, opts api.ContainerLogOpts) (*string, error)
type ExecuteContainerCommandFunc func(ctx context.Context, resourceGroup, cgName, containerName string, containerReq azaci.ContainerExecRequest) (azaci.ContainerExecResponse, error)

type GetContainerGroupFunc func(ctx context.Context, resourceGroup, containerGroupName string) (*client.ContainerGroupWrapper, error)

type MockACIProvider struct {
	MockCreateContainerGroup    CreateContainerGroupFunc
	MockGetContainerGroupInfo   GetContainerGroupInfoFunc
	MockGetContainerGroupList   GetContainerGroupListFunc
	MockListCapabilities        ListCapabilitiesFunc
	MockDeleteContainerGroup    DeleteContainerGroupFunc
	MockListLogs                ListLogsFunc
	MockExecuteContainerCommand ExecuteContainerCommandFunc

	MockGetContainerGroup GetContainerGroupFunc
}

func NewMockACIProvider(capList ListCapabilitiesFunc) *MockACIProvider {
	mock := &MockACIProvider{}
	mock.MockListCapabilities = capList
	return mock
}

func (m *MockACIProvider) ListCapabilities(ctx context.Context, region string) (*[]azaci.Capabilities, error) {
	if m.MockListCapabilities != nil {
		return m.MockListCapabilities(ctx, region)
	}
	return nil, nil
}

func (m *MockACIProvider) GetContainerGroupListResult(ctx context.Context, resourcegroup string) (*[]azaci.ContainerGroup, error) {
	if m.MockGetContainerGroupList != nil {
		return m.MockGetContainerGroupList(ctx, resourcegroup)
	}
	return nil, nil
}

func (m *MockACIProvider) GetContainerGroupInfo(ctx context.Context, resourceGroup, namespace, name, nodeName string) (*azaci.ContainerGroup, error) {
	if m.MockGetContainerGroupInfo != nil {
		return m.MockGetContainerGroupInfo(ctx, resourceGroup, namespace, name, nodeName)
	}
	return nil, nil
}

func (m *MockACIProvider) CreateContainerGroup(ctx context.Context, resourceGroup, podNS, podName string, cg *client.ContainerGroupWrapper) error {
	if m.MockCreateContainerGroup != nil {
		return m.MockCreateContainerGroup(ctx, resourceGroup, podNS, podName, cg)
	}
	return nil
}
func (m *MockACIProvider) DeleteContainerGroup(ctx context.Context, resourceGroup, cgName string) error {
	if m.MockDeleteContainerGroup != nil {
		return m.MockDeleteContainerGroup(ctx, resourceGroup, cgName)
	}
	return nil
}

func (m *MockACIProvider) ListLogs(ctx context.Context, resourceGroup, cgName, containerName string, opts api.ContainerLogOpts) (*string, error) {
	if m.MockListLogs != nil {
		return m.MockListLogs(ctx, resourceGroup, cgName, containerName, opts)
	}
	return nil, nil
}

func (m *MockACIProvider) ExecuteContainerCommand(ctx context.Context, resourceGroup, cgName, containerName string, containerReq azaci.ContainerExecRequest) (*azaci.ContainerExecResponse, error) {
	if m.MockExecuteContainerCommand != nil {
		result, err := m.MockExecuteContainerCommand(ctx, resourceGroup, cgName, containerName, containerReq)
		return &result, err
	}
	return nil, nil
}

func (m *MockACIProvider) GetContainerGroup(ctx context.Context, resourceGroup, containerGroupName string) (*client.ContainerGroupWrapper, error) {
	if m.MockGetContainerGroup != nil {
		return m.MockGetContainerGroup(ctx, resourceGroup, containerGroupName)
	}
	return nil, nil
}
