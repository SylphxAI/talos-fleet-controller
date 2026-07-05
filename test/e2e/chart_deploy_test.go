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

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
)

const (
	// namespace is the isolated namespace the chart release is installed into.
	namespace = "tfc-e2e"

	// releaseName does not contain the chart name, so the chart's fullname
	// helper yields "<releaseName>-talos-fleet-controller".
	releaseName = "tfc-e2e"
	fullname    = releaseName + "-talos-fleet-controller"

	imageRepo = "ghcr.io/sylphxai/talos-fleet-controller"
	imageTag  = "e2e"

	// defaultKindCluster matches KIND_CLUSTER in the Makefile.
	defaultKindCluster = "talos-fleet-controller-test-e2e"
)

// talosconfigTemplate is a syntactically valid talosconfig. The Talos client
// only parses it at startup (gRPC connections are lazy), so generated
// self-signed credentials are enough for the binary to boot without a live
// Talos endpoint.
const talosconfigTemplate = `context: e2e
contexts:
  e2e:
    endpoints:
      - 127.0.0.1
    ca: %s
    crt: %s
    key: %s
`

// run executes a command from the repository root and returns combined output.
func run(name string, args ...string) (string, error) {
	root, err := filepath.Abs("../..")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	ginkgo.GinkgoWriter.Printf("$ %s %v\n%s\n", name, args, string(out))
	return string(out), err
}

// mustRun executes a command and fails the spec on a non-zero exit.
func mustRun(name string, args ...string) string {
	out, err := run(name, args...)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred(), out)
	return out
}

// kubeContext pins every kubectl/helm call to the kind cluster. Relying on
// the ambient current-context is unacceptable: it could point at a real
// cluster. Set in BeforeAll.
var kubeContext string

// kubectl runs kubectl pinned to the kind context, scoped to the e2e namespace.
func kubectl(args ...string) string {
	return mustRun("kubectl", append([]string{"--context", kubeContext, "-n", namespace}, args...)...)
}

// generateSelfSignedCert returns a self-signed certificate + key in PEM form,
// usable both as a CA bundle and as a leaf certificate.
func generateSelfSignedCert(commonName string, dnsNames []string) (certPEM, keyPEM []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())
	keyDER, err := x509.MarshalECPrivateKey(key)
	gomega.ExpectWithOffset(1, err).NotTo(gomega.HaveOccurred())

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// The regression this suite guards against: chart templates rendering flags
// the binary does not have (e.g. controller-manager-era --leader-elect),
// which surfaces in production as CrashLoopBackOff. Installing the chart
// against the freshly built image proves args, probes, cert mount, and
// talosconfig wiring all match the real binary.
var _ = ginkgo.Describe("Helm chart deployment", ginkgo.Ordered, func() {
	ginkgo.BeforeAll(func() {
		kindCluster := os.Getenv("KIND_CLUSTER")
		if kindCluster == "" {
			kindCluster = defaultKindCluster
		}
		kindBin := os.Getenv("KIND")
		if kindBin == "" {
			kindBin = "kind"
		}
		kubeContext = "kind-" + kindCluster

		ginkgo.By("building the controller image")
		mustRun("docker", "build", "-t", imageRepo+":"+imageTag, ".")

		ginkgo.By("loading the image into the kind cluster")
		mustRun(kindBin, "load", "docker-image", imageRepo+":"+imageTag, "--name", kindCluster)

		ginkgo.By("creating the namespace")
		_, _ = run("kubectl", "--context", kubeContext, "delete", "namespace", namespace,
			"--ignore-not-found", "--wait")
		mustRun("kubectl", "--context", kubeContext, "create", "namespace", namespace)

		ginkgo.By("creating the webhook serving-cert Secret (manual cert flow)")
		dir := ginkgo.GinkgoT().TempDir()
		servingDNS := fullname + "-webhook." + namespace + ".svc"
		servingCert, servingKey := generateSelfSignedCert(servingDNS, []string{servingDNS})
		certPath := filepath.Join(dir, "tls.crt")
		keyPath := filepath.Join(dir, "tls.key")
		gomega.Expect(os.WriteFile(certPath, servingCert, 0o600)).To(gomega.Succeed())
		gomega.Expect(os.WriteFile(keyPath, servingKey, 0o600)).To(gomega.Succeed())
		kubectl("create", "secret", "tls", fullname+"-cert", "--cert", certPath, "--key", keyPath)

		ginkgo.By("creating the talosconfig Secret the talos.dev ServiceAccount would provide")
		clientCert, clientKey := generateSelfSignedCert("tfc-e2e-talos-client", nil)
		talosconfig := fmt.Sprintf(talosconfigTemplate,
			base64.StdEncoding.EncodeToString(clientCert),
			base64.StdEncoding.EncodeToString(clientCert),
			base64.StdEncoding.EncodeToString(clientKey),
		)
		talosconfigPath := filepath.Join(dir, "talosconfig")
		gomega.Expect(os.WriteFile(talosconfigPath, []byte(talosconfig), 0o600)).To(gomega.Succeed())
		kubectl("create", "secret", "generic", "tfc-talos-api", "--from-file=config="+talosconfigPath)

		ginkgo.By("installing the Helm chart")
		setValues := []string{
			"namespace.create=false",
			"namespace.name=" + namespace,
			"certManager.enabled=false",
			"talosServiceAccount.create=false",
			"image.repository=" + imageRepo,
			"image.tag=" + imageTag,
		}
		installArgs := make([]string, 0, 7+2*len(setValues))
		installArgs = append(installArgs,
			"install", releaseName, "charts/talos-fleet-controller",
			"--kube-context", kubeContext, "--namespace", namespace)
		for _, set := range setValues {
			installArgs = append(installArgs, "--set", set)
		}
		mustRun("helm", installArgs...)
	})

	ginkgo.AfterAll(func() {
		_, _ = run("helm", "uninstall", releaseName, "--kube-context", kubeContext, "--namespace", namespace)
		_, _ = run("kubectl", "--context", kubeContext, "delete", "namespace", namespace,
			"--ignore-not-found", "--wait=false")
	})

	ginkgo.It("brings the extension server Deployment to Available", func() {
		kubectl("rollout", "status", "deployment/"+fullname, "--timeout=180s")
	})

	ginkgo.It("logs a successful extension server startup", func() {
		logs := kubectl("logs", "deployment/"+fullname)
		gomega.Expect(logs).To(gomega.ContainSubstring("Starting Talos In-Place Update Extension server"))
	})
})
