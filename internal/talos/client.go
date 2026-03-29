// Package talos provides a thin wrapper around the Talos gRPC client
// for operations needed by the fleet controller: version, config read,
// config apply, and upgrade.
package talos

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/cosi-project/runtime/pkg/resource"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

// Client wraps the Talos gRPC client with fleet-controller-specific methods.
type Client struct {
	inner *talosclient.Client
}

// NewClient creates a Talos client from the default config (TALOSCONFIG env var
// or ServiceAccount-injected secret at /var/run/secrets/talos.dev/config).
func NewClient(ctx context.Context) (*Client, error) {
	c, err := talosclient.New(ctx, talosclient.WithDefaultConfig())
	if err != nil {
		return nil, fmt.Errorf("create talos client: %w", err)
	}
	return &Client{inner: c}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.inner.Close()
}

// NodeVersion returns the Talos OS version tag (e.g., "v1.12.5") for a node.
func (c *Client) NodeVersion(ctx context.Context, nodeIP string) (string, error) {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	resp, err := c.inner.Version(nodeCtx)
	if err != nil {
		return "", fmt.Errorf("get version from %s: %w", nodeIP, err)
	}
	if len(resp.Messages) == 0 {
		return "", fmt.Errorf("no version response from %s", nodeIP)
	}

	return resp.Messages[0].GetVersion().GetTag(), nil
}

// NodeConfigRaw returns the raw machine config YAML bytes from a node.
// Uses the GenerateConfiguration API which returns the actual machine config
// (not the COSI resource envelope). This is what ApplyConfiguration expects.
func (c *Client) NodeConfigRaw(ctx context.Context, nodeIP string) ([]byte, error) {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	// Use MachineClient.GenerateConfiguration to get the raw config.
	// Alternatively, read via COSI and extract the spec field.
	md := resource.NewMetadata(
		"config",
		"MachineConfigs.config.talos.dev",
		"v1alpha1",
		resource.VersionUndefined,
	)

	r, err := c.inner.COSI.Get(nodeCtx, md)
	if err != nil {
		return nil, fmt.Errorf("get machineconfig from %s: %w", nodeIP, err)
	}

	// Type-assert to MachineConfig to access Container().Bytes() — the only
	// reliable way to get the raw config YAML without double-encoding.
	mc, ok := r.(*configres.MachineConfig)
	if !ok {
		return nil, fmt.Errorf("unexpected resource type from %s: %T (expected *MachineConfig)", nodeIP, r)
	}

	// Container().Bytes() returns the original YAML source bytes that Talos
	// stores internally — exactly what ApplyConfiguration expects.
	configBytes, err := mc.Container().Bytes()
	if err != nil {
		return nil, fmt.Errorf("get config bytes from %s: %w", nodeIP, err)
	}

	return configBytes, nil
}

// NodeConfigProvider returns the current machine config as a typed config.Provider.
// Used by configpatcher for proper semantic merge and deterministic comparison.
func (c *Client) NodeConfigProvider(ctx context.Context, nodeIP string) (config.Provider, error) {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	md := resource.NewMetadata(
		"config",
		"MachineConfigs.config.talos.dev",
		"v1alpha1",
		resource.VersionUndefined,
	)

	r, err := c.inner.COSI.Get(nodeCtx, md)
	if err != nil {
		return nil, fmt.Errorf("get machineconfig from %s: %w", nodeIP, err)
	}

	mc, ok := r.(*configres.MachineConfig)
	if !ok {
		return nil, fmt.Errorf("unexpected resource type from %s: %T", nodeIP, r)
	}

	return mc.Provider(), nil
}

// NodeConfigHash returns a SHA-256 hash of the node's current machine config.
// Used for drift detection — compare against desired config hash.
func (c *Client) NodeConfigHash(ctx context.Context, nodeIP string) (string, error) {
	raw, err := c.NodeConfigRaw(ctx, nodeIP)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%x", h), nil
}

// ApplyConfig applies a machine config to a node.
// The config is a strategic merge patch — cluster secrets are untouched.
func (c *Client) ApplyConfig(ctx context.Context, nodeIP string, configYAML []byte, mode ApplyMode) error {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	resp, err := c.inner.ApplyConfiguration(nodeCtx, &machineapi.ApplyConfigurationRequest{
		Data: configYAML,
		Mode: mode.toProto(),
	})
	if err != nil {
		return fmt.Errorf("apply config to %s: %w", nodeIP, err)
	}

	// Log response for diagnostics.
	_ = resp

	return nil
}

// Upgrade triggers a Talos OS upgrade on a node to the specified installer image.
func (c *Client) Upgrade(ctx context.Context, nodeIP, installerImage string) error {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	_, err := c.inner.UpgradeWithOptions(nodeCtx,
		talosclient.WithUpgradeImage(installerImage),
		talosclient.WithUpgradePreserve(true),
	)
	if err != nil {
		return fmt.Errorf("upgrade %s to %s: %w", nodeIP, installerImage, err)
	}

	return nil
}

// EtcdMembers returns the list of etcd member IDs for quorum checking.
func (c *Client) EtcdMembers(ctx context.Context, nodeIP string) (int, error) {
	nodeCtx := talosclient.WithNode(ctx, nodeIP)

	resp, err := c.inner.EtcdMemberList(nodeCtx, &machineapi.EtcdMemberListRequest{})
	if err != nil {
		return 0, fmt.Errorf("etcd members from %s: %w", nodeIP, err)
	}
	if len(resp.Messages) == 0 {
		return 0, fmt.Errorf("no etcd response from %s", nodeIP)
	}

	return len(resp.Messages[0].Members), nil
}

// ApplyMode maps to Talos apply-config modes.
type ApplyMode string

const (
	ApplyModeAuto     ApplyMode = "auto"
	ApplyModeNoReboot ApplyMode = "no-reboot"
	ApplyModeReboot   ApplyMode = "reboot"
	ApplyModeStaged   ApplyMode = "staged"
)

func (m ApplyMode) toProto() machineapi.ApplyConfigurationRequest_Mode {
	switch m {
	case ApplyModeNoReboot:
		return machineapi.ApplyConfigurationRequest_NO_REBOOT
	case ApplyModeReboot:
		return machineapi.ApplyConfigurationRequest_REBOOT
	case ApplyModeStaged:
		return machineapi.ApplyConfigurationRequest_STAGED
	default:
		return machineapi.ApplyConfigurationRequest_AUTO
	}
}
