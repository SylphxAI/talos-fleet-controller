/*
Copyright 2026 SylphxAI.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package talos

import (
	"context"
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config"
)

// Compile-time assertion that MockClient implements Interface.
var _ Interface = (*MockClient)(nil)

// MockClient implements the Talos client interface for testing.
// All methods are configurable via function fields. When a function field
// is nil, the corresponding method returns a sensible zero value or error.
type MockClient struct {
	NodeVersionFunc        func(ctx context.Context, nodeIP string) (string, error)
	NodeConfigRawFunc      func(ctx context.Context, nodeIP string) ([]byte, error)
	NodeConfigProviderFunc func(ctx context.Context, nodeIP string) (config.Provider, error)
	NodeConfigHashFunc     func(ctx context.Context, nodeIP string) (string, error)
	ApplyConfigFunc        func(ctx context.Context, nodeIP string, configYAML []byte, mode ApplyMode) error
	UpgradeFunc            func(ctx context.Context, nodeIP, installerImage string) error
	EtcdMembersFunc        func(ctx context.Context, nodeIP string) (int, error)
	CloseFunc              func() error
}

// NodeVersion returns the Talos OS version for a node.
func (m *MockClient) NodeVersion(ctx context.Context, nodeIP string) (string, error) {
	if m.NodeVersionFunc != nil {
		return m.NodeVersionFunc(ctx, nodeIP)
	}
	return "", fmt.Errorf("MockClient.NodeVersionFunc not configured")
}

// NodeConfigRaw returns the raw machine config YAML bytes from a node.
func (m *MockClient) NodeConfigRaw(ctx context.Context, nodeIP string) ([]byte, error) {
	if m.NodeConfigRawFunc != nil {
		return m.NodeConfigRawFunc(ctx, nodeIP)
	}
	return nil, fmt.Errorf("MockClient.NodeConfigRawFunc not configured")
}

// NodeConfigProvider returns the current machine config as a typed config.Provider.
func (m *MockClient) NodeConfigProvider(ctx context.Context, nodeIP string) (config.Provider, error) {
	if m.NodeConfigProviderFunc != nil {
		return m.NodeConfigProviderFunc(ctx, nodeIP)
	}
	return nil, fmt.Errorf("MockClient.NodeConfigProviderFunc not configured")
}

// NodeConfigHash returns a SHA-256 hash of the node's current machine config.
func (m *MockClient) NodeConfigHash(ctx context.Context, nodeIP string) (string, error) {
	if m.NodeConfigHashFunc != nil {
		return m.NodeConfigHashFunc(ctx, nodeIP)
	}
	return "", fmt.Errorf("MockClient.NodeConfigHashFunc not configured")
}

// ApplyConfig applies a machine config to a node.
func (m *MockClient) ApplyConfig(ctx context.Context, nodeIP string, configYAML []byte, mode ApplyMode) error {
	if m.ApplyConfigFunc != nil {
		return m.ApplyConfigFunc(ctx, nodeIP, configYAML, mode)
	}
	return fmt.Errorf("MockClient.ApplyConfigFunc not configured")
}

// Upgrade triggers a Talos OS upgrade on a node.
func (m *MockClient) Upgrade(ctx context.Context, nodeIP, installerImage string) error {
	if m.UpgradeFunc != nil {
		return m.UpgradeFunc(ctx, nodeIP, installerImage)
	}
	return fmt.Errorf("MockClient.UpgradeFunc not configured")
}

// EtcdMembers returns the number of etcd members for quorum checking.
func (m *MockClient) EtcdMembers(ctx context.Context, nodeIP string) (int, error) {
	if m.EtcdMembersFunc != nil {
		return m.EtcdMembersFunc(ctx, nodeIP)
	}
	return 0, fmt.Errorf("MockClient.EtcdMembersFunc not configured")
}

// Close closes the mock client (no-op unless CloseFunc is set).
func (m *MockClient) Close() error {
	if m.CloseFunc != nil {
		return m.CloseFunc()
	}
	return nil
}
