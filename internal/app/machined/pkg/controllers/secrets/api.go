// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package secrets

import (
	"context"
	stdlibx509 "crypto/x509"
	"fmt"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/crypto/x509"
	"github.com/siderolabs/go-pointer"
	"go.uber.org/zap"

	"github.com/siderolabs/talos/pkg/grpc/gen"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1/machine"
	"github.com/siderolabs/talos/pkg/machinery/resources/config"
	"github.com/siderolabs/talos/pkg/machinery/resources/k8s"
	"github.com/siderolabs/talos/pkg/machinery/resources/network"
	"github.com/siderolabs/talos/pkg/machinery/resources/secrets"
	timeresource "github.com/siderolabs/talos/pkg/machinery/resources/time"
	"github.com/siderolabs/talos/pkg/machinery/resources/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/role"
)

// APIController manages secrets.API based on configuration to provide apid certificate.
type APIController struct{}

// Name implements controller.Controller interface.
func (ctrl *APIController) Name() string {
	return "secrets.APIController"
}

// Inputs implements controller.Controller interface.
func (ctrl *APIController) Inputs() []controller.Input {
	// initial set of inputs: wait for machine type to be known and network to be partially configured
	return []controller.Input{
		{
			Namespace: network.NamespaceName,
			Type:      network.StatusType,
			ID:        pointer.To(network.StatusID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineTypeType,
			ID:        pointer.To(config.MachineTypeID),
			Kind:      controller.InputWeak,
		},
	}
}

// Outputs implements controller.Controller interface.
func (ctrl *APIController) Outputs() []controller.Output {
	return []controller.Output{
		{
			Type: secrets.APIType,
			Kind: controller.OutputExclusive,
		},
	}
}

// Run implements controller.Controller interface.
//
//nolint:gocyclo
func (ctrl *APIController) Run(ctx context.Context, r controller.Runtime, logger *zap.Logger) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		}

		// reset inputs back to what they were initially
		if err := r.UpdateInputs(ctrl.Inputs()); err != nil {
			return err
		}

		machineTypeRes, err := r.Get(ctx, resource.NewMetadata(config.NamespaceName, config.MachineTypeType, config.MachineTypeID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				continue
			}

			return fmt.Errorf("error getting machine type: %w", err)
		}

		machineType := machineTypeRes.(*config.MachineType).MachineType()

		networkResource, err := r.Get(ctx, resource.NewMetadata(network.NamespaceName, network.StatusType, network.StatusID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				continue
			}

			return err
		}

		networkStatus := networkResource.(*network.Status).TypedSpec()

		if !(networkStatus.AddressReady && networkStatus.HostnameReady) {
			continue
		}

		// machine type is known and network is ready, we can now proceed to one or another reconcile loop
		switch machineType {
		case machine.TypeInit, machine.TypeControlPlane:
			if err = ctrl.reconcile(ctx, r, logger, true); err != nil {
				return err
			}
		case machine.TypeWorker:
			if err = ctrl.reconcile(ctx, r, logger, false); err != nil {
				return err
			}
		case machine.TypeUnknown:
			// machine configuration is not loaded yet, do nothing
		default:
			panic(fmt.Sprintf("unexpected machine type %v", machineType))
		}

		if err = ctrl.teardownAll(ctx, r); err != nil {
			return err
		}
	}
}

//nolint:gocyclo,cyclop,dupl
func (ctrl *APIController) reconcile(ctx context.Context, r controller.Runtime, logger *zap.Logger, isControlplane bool) error {
	inputs := []controller.Input{
		{
			Namespace: secrets.NamespaceName,
			Type:      secrets.OSRootType,
			ID:        pointer.To(secrets.OSRootID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: secrets.NamespaceName,
			Type:      secrets.CertSANType,
			ID:        pointer.To(secrets.CertSANAPIID),
			Kind:      controller.InputWeak,
		},
		{
			Namespace: config.NamespaceName,
			Type:      config.MachineTypeType,
			ID:        pointer.To(config.MachineTypeID),
			Kind:      controller.InputWeak,
		},
		// time status isn't fetched, but the fact that it is in dependencies means
		// that certs will be regenerated on time sync/jump (as reconcile will be triggered)
		{
			Namespace: v1alpha1.NamespaceName,
			Type:      timeresource.StatusType,
			ID:        pointer.To(timeresource.StatusID),
			Kind:      controller.InputWeak,
		},
	}

	if !isControlplane {
		// worker nodes depend on endpoint list
		inputs = append(inputs, controller.Input{
			Namespace: k8s.ControlPlaneNamespaceName,
			Type:      k8s.EndpointType,
			Kind:      controller.InputWeak,
		})
	}

	if err := r.UpdateInputs(inputs); err != nil {
		return fmt.Errorf("error updating inputs: %w", err)
	}

	r.QueueReconcile()

	refreshTicker := time.NewTicker(x509.DefaultCertificateValidityDuration / 2)
	defer refreshTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.EventCh():
		case <-refreshTicker.C:
		}

		machineTypeRes, err := r.Get(ctx, resource.NewMetadata(config.NamespaceName, config.MachineTypeType, config.MachineTypeID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				continue
			}

			return fmt.Errorf("error getting machine type: %w", err)
		}

		machineType := machineTypeRes.(*config.MachineType).MachineType()

		switch machineType {
		case machine.TypeInit, machine.TypeControlPlane:
			if !isControlplane {
				return fmt.Errorf("machine type changed")
			}
		case machine.TypeWorker:
			if isControlplane {
				return fmt.Errorf("machine type changed")
			}
		case machine.TypeUnknown:
			return fmt.Errorf("machine type changed")
		default:
			panic(fmt.Sprintf("unexpected machine type %v", machineType))
		}

		rootResource, err := r.Get(ctx, resource.NewMetadata(secrets.NamespaceName, secrets.OSRootType, secrets.OSRootID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				if err = ctrl.teardownAll(ctx, r); err != nil {
					return fmt.Errorf("error destroying resources: %w", err)
				}

				continue
			}

			return fmt.Errorf("error getting etcd root secrets: %w", err)
		}

		rootSpec := rootResource.(*secrets.OSRoot).TypedSpec()

		certSANResource, err := r.Get(ctx, resource.NewMetadata(secrets.NamespaceName, secrets.CertSANType, secrets.CertSANAPIID, resource.VersionUndefined))
		if err != nil {
			if state.IsNotFoundError(err) {
				continue
			}

			return fmt.Errorf("error getting certSANs: %w", err)
		}

		certSANs := certSANResource.(*secrets.CertSAN).TypedSpec()

		var endpointsStr []string

		if !isControlplane {
			endpointResources, err := r.List(ctx, resource.NewMetadata(k8s.ControlPlaneNamespaceName, k8s.EndpointType, "", resource.VersionUndefined))
			if err != nil {
				return fmt.Errorf("error getting endpoints resources: %w", err)
			}

			var endpointAddrs k8s.EndpointList

			// merge all endpoints into a single list
			for _, res := range endpointResources.Items {
				endpointAddrs = endpointAddrs.Merge(res.(*k8s.Endpoint))
			}

			if len(endpointAddrs) == 0 {
				continue
			}

			endpointsStr = endpointAddrs.Strings()
		}

		if isControlplane {
			if err := ctrl.generateControlPlane(ctx, r, logger, rootSpec, certSANs); err != nil {
				return err
			}
		} else {
			if err := ctrl.generateWorker(ctx, r, logger, rootSpec, endpointsStr, certSANs); err != nil {
				return err
			}
		}
	}
}

func (ctrl *APIController) generateControlPlane(ctx context.Context, r controller.Runtime, logger *zap.Logger, rootSpec *secrets.OSRootSpec, certSANs *secrets.CertSANSpec) error {
	ca, err := x509.NewCertificateAuthorityFromCertificateAndKey(rootSpec.CA)
	if err != nil {
		return fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	serverCert, err := x509.NewKeyPair(ca,
		x509.IPAddresses(certSANs.StdIPs()),
		x509.DNSNames(certSANs.DNSNames),
		x509.CommonName(certSANs.FQDN),
		x509.NotAfter(time.Now().Add(x509.DefaultCertificateValidityDuration)),
		x509.KeyUsage(stdlibx509.KeyUsageDigitalSignature),
		x509.ExtKeyUsage([]stdlibx509.ExtKeyUsage{
			stdlibx509.ExtKeyUsageServerAuth,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to generate API server cert: %w", err)
	}

	clientCert, err := x509.NewKeyPair(ca,
		x509.CommonName(certSANs.FQDN),
		x509.Organization(string(role.Impersonator)),
		x509.NotAfter(time.Now().Add(x509.DefaultCertificateValidityDuration)),
		x509.KeyUsage(stdlibx509.KeyUsageDigitalSignature),
		x509.ExtKeyUsage([]stdlibx509.ExtKeyUsage{
			stdlibx509.ExtKeyUsageClientAuth,
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to generate API client cert: %w", err)
	}

	if err := r.Modify(ctx, secrets.NewAPI(),
		func(r resource.Resource) error {
			apiSecrets := r.(*secrets.API).TypedSpec()

			apiSecrets.CA = &x509.PEMEncodedCertificateAndKey{
				Crt: rootSpec.CA.Crt,
			}
			apiSecrets.Server = x509.NewCertificateAndKeyFromKeyPair(serverCert)
			apiSecrets.Client = x509.NewCertificateAndKeyFromKeyPair(clientCert)

			return nil
		}); err != nil {
		return fmt.Errorf("error modifying resource: %w", err)
	}

	clientFingerprint, _ := x509.SPKIFingerprintFromDER(clientCert.Certificate.Certificate[0]) //nolint:errcheck
	serverFingerprint, _ := x509.SPKIFingerprintFromDER(serverCert.Certificate.Certificate[0]) //nolint:errcheck

	logger.Debug("generated new certificates",
		zap.Stringer("client", clientFingerprint),
		zap.Stringer("server", serverFingerprint),
	)

	return nil
}

func (ctrl *APIController) generateWorker(ctx context.Context, r controller.Runtime, logger *zap.Logger,
	rootSpec *secrets.OSRootSpec, endpointsStr []string, certSANs *secrets.CertSANSpec,
) error {
	remoteGen, err := gen.NewRemoteGenerator(rootSpec.Token, endpointsStr, rootSpec.CA)
	if err != nil {
		return fmt.Errorf("failed creating trustd client: %w", err)
	}

	defer remoteGen.Close() //nolint:errcheck

	serverCSR, serverCert, err := x509.NewEd25519CSRAndIdentity(
		x509.IPAddresses(certSANs.StdIPs()),
		x509.DNSNames(certSANs.DNSNames),
		x509.CommonName(certSANs.FQDN),
	)
	if err != nil {
		return fmt.Errorf("failed to generate API server CSR: %w", err)
	}

	logger.Debug("sending CSR", zap.Strings("endpoints", endpointsStr))

	var ca []byte

	ca, serverCert.Crt, err = remoteGen.IdentityContext(ctx, serverCSR)
	if err != nil {
		return fmt.Errorf("failed to sign API server CSR: %w", err)
	}

	if err := r.Modify(ctx, secrets.NewAPI(),
		func(r resource.Resource) error {
			apiSecrets := r.(*secrets.API).TypedSpec()

			apiSecrets.CA = &x509.PEMEncodedCertificateAndKey{
				Crt: ca,
			}
			apiSecrets.Server = serverCert

			return nil
		}); err != nil {
		return fmt.Errorf("error modifying resource: %w", err)
	}

	serverFingerprint, _ := x509.SPKIFingerprintFromPEM(serverCert.Crt) //nolint:errcheck

	logger.Debug("generated new certificates",
		zap.Stringer("server", serverFingerprint),
	)

	return nil
}

func (ctrl *APIController) teardownAll(ctx context.Context, r controller.Runtime) error {
	list, err := r.List(ctx, resource.NewMetadata(secrets.NamespaceName, secrets.APIType, "", resource.VersionUndefined))
	if err != nil {
		return err
	}

	for _, res := range list.Items {
		if err = r.Destroy(ctx, res.Metadata()); err != nil {
			return err
		}
	}

	return nil
}
