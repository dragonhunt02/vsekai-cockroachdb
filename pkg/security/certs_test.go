// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package security_test

import (
	"bytes"
	"context"
	"crypto/x509"
	gosql "database/sql"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/security/securitytest"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

const testKeySize = 1024

// tempDir is like testutils.TempDir but avoids a circular import.
func tempDir(t *testing.T) (string, func()) {
	certsDir, err := ioutil.TempDir("", "certs_test")
	if err != nil {
		t.Fatal(err)
	}
	return certsDir, func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerateCACert(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()

	certsDir, cleanup := tempDir(t)
	defer cleanup()

	cm, err := security.NewCertificateManager(certsDir, security.CommandTLSSettings{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	keyPath := filepath.Join(certsDir, "ca.key")

	testCases := []struct {
		certsDir, caKey       string
		allowReuse, overwrite bool
		errStr                string // error string for CreateCAPair, empty for nil.
		numCerts              int    // number of certificates found in ca.crt
	}{
		{"", "ca.key", false, false, "the path to the certs directory is required", 0},
		{certsDir, "", false, false, "the path to the CA key is required", 0},
		// New CA key/cert.
		{certsDir, keyPath, false, false, "", 1},
		// Files exist, but reuse is disabled.
		{certsDir, keyPath, false, false, "exists, but key reuse is disabled", 2},
		// Files exist, but overwrite is off.
		{certsDir, keyPath, true, false, "file exists", 2},
		// Files exist and reuse/overwrite is enabled.
		{certsDir, keyPath, true, true, "", 2},
		// Cert exists and overwrite is enabled.
		{certsDir, keyPath + "2", false, true, "", 3}, // Using a new key still keeps the ca.crt
	}

	for i, tc := range testCases {
		err := security.CreateCAPair(tc.certsDir, tc.caKey, testKeySize,
			time.Hour*48, tc.allowReuse, tc.overwrite)
		if !testutils.IsError(err, tc.errStr) {
			t.Errorf("#%d: expected error %s but got %+v", i, tc.errStr, err)
			continue
		}

		if err != nil {
			continue
		}

		// No failures on CreateCAPair, we expect a valid CA cert.
		err = cm.LoadCertificates()
		if err != nil {
			t.Fatalf("#%d: unexpected failure: %v", i, err)
		}

		ci := cm.CACert()
		if ci == nil {
			t.Fatalf("#%d: no CA cert found", i)
		}

		certs, err := security.PEMToCertificates(ci.FileContents)
		if err != nil {
			t.Fatalf("#%d: unexpected parsing error for %+v: %v", i, ci, err)
		}

		if actual := len(certs); actual != tc.numCerts {
			t.Errorf("#%d: expected %d certificates, found %d", i, tc.numCerts, actual)
		}
	}
}

func TestGenerateTenantCerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()

	certsDir, cleanup := tempDir(t)
	defer cleanup()

	caKeyFile := filepath.Join(certsDir, "name-must-not-matter.key")
	require.NoError(t, security.CreateTenantCAPair(
		certsDir,
		caKeyFile,
		testKeySize,
		48*time.Hour,
		false, // allowKeyReuse
		false, // overwrite
	))

	cp, err := security.CreateTenantPair(
		certsDir,
		caKeyFile,
		testKeySize,
		time.Hour,
		999,
		[]string{"127.0.0.1"},
	)
	require.NoError(t, err)
	require.NoError(t, security.WriteTenantPair(certsDir, cp, false))

	require.NoError(t, security.CreateTenantSigningPair(certsDir, time.Hour, false /* overwrite */, 999))

	cl := security.NewCertificateLoader(certsDir)
	require.NoError(t, cl.Load())
	infos := cl.Certificates()
	for _, info := range infos {
		require.NoError(t, info.Error)
	}

	for i := range infos {
		// Scrub the struct to retain only tested fields.
		*infos[i] = security.CertInfo{
			FileUsage: infos[i].FileUsage,
			Filename:  infos[i].Filename,
			Name:      infos[i].Name,
		}
	}
	require.Equal(t, []*security.CertInfo{
		{
			FileUsage: security.TenantCAPem,
			Filename:  "ca-client-tenant.crt",
		},
		{
			FileUsage: security.TenantPem,
			Filename:  "client-tenant.999.crt",
			Name:      "999",
		},
		{
			FileUsage: security.TenantSigningPem,
			Filename:  "tenant-signing.999.crt",
			Name:      "999",
		},
	}, infos)
}

// TestGenerateClientCerts tests tenant scoped client certificates have the username
// set correctly and also have the tenant ID embedded as a SAN.
func TestGenerateClientCerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()

	certsDir := t.TempDir()

	caKeyFile := certsDir + "/ca.key"
	// Generate CA key and crt.
	require.NoError(t, security.CreateCAPair(certsDir, caKeyFile, testKeySize,
		time.Hour*72, false /* allowReuse */, false /* overwrite */))
	user := security.MakeSQLUsernameFromPreNormalizedString("user")
	tenantIDs := []roachpb.TenantID{roachpb.SystemTenantID, roachpb.MakeTenantID(123)}
	// Create tenant-scoped client cert.
	require.NoError(t, security.CreateClientPair(
		certsDir,
		caKeyFile,
		testKeySize,
		48*time.Hour,
		false, /*overwrite */
		user,
		tenantIDs,
		false /* wantPKCS8Key */))

	// Load and verify the certificates.
	cl := security.NewCertificateLoader(certsDir)
	require.NoError(t, cl.Load())
	infos := cl.Certificates()
	for _, info := range infos {
		require.NoError(t, info.Error)
	}

	// We expect two certificates: the CA certificate and the tenant scoped client certificate.
	require.Equal(t, 2, len(infos))
	expectedClientCrtName := fmt.Sprintf("client.%s.crt", user)
	expectedSANs, err := security.MakeTenantURISANs(user, tenantIDs)
	require.NoError(t, err)
	for _, info := range infos {
		if info.Filename == "ca.crt" {
			continue
		}
		require.Equal(t, security.ClientPem, info.FileUsage)
		require.Equal(t, expectedClientCrtName, info.Filename)
		require.Equal(t, 1, len(info.ParsedCertificates))
		require.Equal(t, len(tenantIDs), len(info.ParsedCertificates[0].URIs))
		require.Equal(t, expectedSANs, info.ParsedCertificates[0].URIs)
	}
}

func TestGenerateNodeCerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()

	certsDir, err := ioutil.TempDir("", "certs_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}()

	// Try generating node certs without CA certs present.
	if err := security.CreateNodePair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		testKeySize, time.Hour*48, false, []string{"localhost"},
	); err == nil {
		t.Fatalf("Expected error, but got none")
	}

	// Now try in the proper order.
	if err := security.CreateCAPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey), testKeySize, time.Hour*96, false, false,
	); err != nil {
		t.Fatalf("Expected success, got %v", err)
	}

	if err := security.CreateNodePair(
		certsDir, filepath.Join(certsDir, security.EmbeddedCAKey),
		testKeySize, time.Hour*48, false, []string{"localhost"},
	); err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
}

// Generate basic certs:
// ca.crt: CA certificate
// node.crt: dual-purpose node certificate
// client.root.crt: client certificate for the root user.
// client-tenant.10.crt: tenant client certificate for tenant 10.
// tenant-signing.10.crt: tenant signing certificate for tenant 10.
func generateBaseCerts(certsDir string) error {
	{
		caKey := filepath.Join(certsDir, security.EmbeddedCAKey)

		if err := security.CreateCAPair(
			certsDir, caKey,
			testKeySize, time.Hour*96, true, true,
		); err != nil {
			return err
		}

		if err := security.CreateNodePair(
			certsDir, caKey,
			testKeySize, time.Hour*48, true, []string{"127.0.0.1"},
		); err != nil {
			return err
		}

		if err := security.CreateClientPair(
			certsDir,
			caKey,
			testKeySize,
			time.Hour*48,
			true,
			security.RootUserName(),
			[]roachpb.TenantID{roachpb.SystemTenantID},
			false,
		); err != nil {
			return err
		}
	}

	{
		tenantID := uint64(10)
		caKey := filepath.Join(certsDir, security.EmbeddedTenantCAKey)
		if err := security.CreateTenantCAPair(
			certsDir, caKey,
			testKeySize, time.Hour*96, true, true,
		); err != nil {
			return err
		}

		tcp, err := security.CreateTenantPair(certsDir, caKey,
			testKeySize, time.Hour*48, tenantID, []string{"127.0.0.1"})
		if err != nil {
			return err
		}
		if err := security.WriteTenantPair(certsDir, tcp, true); err != nil {
			return err
		}
		if err := security.CreateTenantSigningPair(certsDir, 96*time.Hour, true /* overwrite */, tenantID); err != nil {
			return err
		}
	}

	return nil
}

// Generate certificates with separate CAs:
// ca.crt: CA certificate
// ca-client.crt: CA certificate to verify client certs
// node.crt: node server cert: signed by ca.crt
// client.node.crt: node client cert: signed by ca-client.crt
// client.root.crt: root client cert: signed by ca-client.crt
func generateSplitCACerts(certsDir string) error {
	if err := generateBaseCerts(certsDir); err != nil {
		return err
	}

	// Overwrite those certs that we want to split.

	if err := security.CreateClientCAPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedClientCAKey),
		testKeySize, time.Hour*96, true, true,
	); err != nil {
		return errors.Wrap(err, "could not generate client CA pair")
	}

	if err := security.CreateClientPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedClientCAKey),
		testKeySize, time.Hour*48, true, security.NodeUserName(), []roachpb.TenantID{roachpb.SystemTenantID}, false,
	); err != nil {
		return errors.Wrap(err, "could not generate Client pair")
	}

	if err := security.CreateClientPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedClientCAKey),
		testKeySize, time.Hour*48, true, security.RootUserName(), []roachpb.TenantID{roachpb.SystemTenantID}, false,
	); err != nil {
		return errors.Wrap(err, "could not generate Client pair")
	}

	if err := security.CreateUICAPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedUICAKey),
		testKeySize, time.Hour*96, true, true,
	); err != nil {
		return errors.Wrap(err, "could not generate UI CA pair")
	}

	if err := security.CreateUIPair(
		certsDir, filepath.Join(certsDir, security.EmbeddedUICAKey),
		testKeySize, time.Hour*48, true, []string{"127.0.0.1"},
	); err != nil {
		return errors.Wrap(err, "could not generate UI pair")
	}

	return nil
}

// This is a fairly high-level test of CA and node certificates.
// We construct SSL server and clients and use the generated certs.
func TestUseCerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()
	certsDir, err := ioutil.TempDir("", "certs_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}()

	if err := generateBaseCerts(certsDir); err != nil {
		t.Fatal(err)
	}

	// Start a test server and override certs.
	// We use a real context since we want generated certs.
	// Web session authentication is disabled in order to avoid the need to
	// authenticate the individual clients being instantiated (session auth has
	// no effect on what is being tested here).
	params := base.TestServerArgs{
		SSLCertsDir:                     certsDir,
		DisableWebSessionAuthentication: true,
	}
	s, _, db := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	// Insecure mode.
	clientContext := testutils.NewNodeTestBaseContext()
	clientContext.Insecure = true
	sCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
	httpClient, err := sCtx.GetHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("Expected SSL error, got success: %s", body)
	}

	// New client. With certs this time.
	clientContext = testutils.NewNodeTestBaseContext()
	clientContext.SSLCertsDir = certsDir
	{
		secondSCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
		httpClient, err = secondSCtx.GetHTTPClient()
	}
	if err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
	req, err = http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("Expected OK, got %q with body: %s", resp.Status, body)
	}

	// Check KV connection.
	if err := db.Put(context.Background(), "foo", "bar"); err != nil {
		t.Error(err)
	}
}

func makeSecurePGUrl(addr, user, certsDir, caName, certName, keyName string) string {
	return fmt.Sprintf("postgresql://%s@%s/?sslmode=verify-full&sslrootcert=%s&sslcert=%s&sslkey=%s",
		user, addr,
		filepath.Join(certsDir, caName),
		filepath.Join(certsDir, certName),
		filepath.Join(certsDir, keyName))
}

// This is a fairly high-level test of CA and node certificates.
// We construct SSL server and clients and use the generated certs.
func TestUseSplitCACerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()
	certsDir, err := ioutil.TempDir("", "certs_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}()

	if err := generateSplitCACerts(certsDir); err != nil {
		t.Fatal(err)
	}

	// Start a test server and override certs.
	// We use a real context since we want generated certs.
	// Web session authentication is disabled in order to avoid the need to
	// authenticate the individual clients being instantiated (session auth has
	// no effect on what is being tested here).
	params := base.TestServerArgs{
		SSLCertsDir:                     certsDir,
		DisableWebSessionAuthentication: true,
	}
	s, _, db := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	// Insecure mode.
	clientContext := testutils.NewNodeTestBaseContext()
	clientContext.Insecure = true
	sCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
	httpClient, err := sCtx.GetHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("Expected SSL error, got success: %s", body)
	}

	// New client. With certs this time.
	clientContext = testutils.NewNodeTestBaseContext()
	clientContext.SSLCertsDir = certsDir
	{
		secondSCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
		httpClient, err = secondSCtx.GetHTTPClient()
	}
	if err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
	req, err = http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}
	resp, err = httpClient.Do(req)
	if err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("Expected OK, got %q with body: %s", resp.Status, body)
	}

	// Check KV connection.
	if err := db.Put(context.Background(), "foo", "bar"); err != nil {
		t.Error(err)
	}

	// Test a SQL client with various certificates.
	testCases := []struct {
		user, caName, certPrefix string
		expectedError            string
	}{
		// Success, but "node" is not a sql user.
		{"node", security.EmbeddedCACert, "client.node", "pq: password authentication failed for user node"},
		// Success!
		{"root", security.EmbeddedCACert, "client.root", ""},
		// Bad server CA: can't verify server certificate.
		{"root", security.EmbeddedClientCACert, "client.root", "certificate signed by unknown authority"},
		// Bad client cert: we're using the node cert but it's not signed by the client CA.
		{"node", security.EmbeddedCACert, "node", "tls: bad certificate"},
		// We can't verify the node certificate using the UI cert.
		{"node", security.EmbeddedUICACert, "node", "certificate signed by unknown authority"},
		// And the SQL server doesn't know what the ui.crt is.
		{"node", security.EmbeddedCACert, "ui", "tls: bad certificate"},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			pgUrl := makeSecurePGUrl(s.ServingSQLAddr(), tc.user, certsDir, tc.caName, tc.certPrefix+".crt", tc.certPrefix+".key")
			goDB, err := gosql.Open("postgres", pgUrl)
			if err != nil {
				t.Fatal(err)
			}
			defer goDB.Close()

			_, err = goDB.Exec("SELECT 1")
			if !testutils.IsError(err, tc.expectedError) {
				t.Errorf("#%d: expected error %v, got %v", i, tc.expectedError, err)
			}
		})
	}
}

// This is a fairly high-level test of CA and node certificates.
// We construct SSL server and clients and use the generated certs.
func TestUseWrongSplitCACerts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// Do not mock cert access for this test.
	security.ResetAssetLoader()
	defer ResetTest()
	certsDir, err := ioutil.TempDir("", "certs_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(certsDir); err != nil {
			t.Fatal(err)
		}
	}()

	if err := generateSplitCACerts(certsDir); err != nil {
		t.Fatal(err)
	}

	// Delete ca-client.crt and ca-ui.crt before starting the node.
	// This will make the server fall back on using ca.crt.
	if err := os.Remove(filepath.Join(certsDir, "ca-client.crt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(certsDir, "ca-ui.crt")); err != nil {
		t.Fatal(err)
	}

	// Start a test server and override certs.
	// We use a real context since we want generated certs.
	// Web session authentication is disabled in order to avoid the need to
	// authenticate the individual clients being instantiated (session auth has
	// no effect on what is being tested here).
	params := base.TestServerArgs{
		SSLCertsDir:                     certsDir,
		DisableWebSessionAuthentication: true,
	}
	s, _, db := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	// Insecure mode.
	clientContext := testutils.NewNodeTestBaseContext()
	clientContext.Insecure = true
	sCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
	httpClient, err := sCtx.GetHTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("Expected SSL error, got success: %s", body)
	}

	// New client with certs, but the UI CA is gone, we have no way to verify the Admin UI cert.
	clientContext = testutils.NewNodeTestBaseContext()
	clientContext.SSLCertsDir = certsDir
	{
		secondCtx := rpc.MakeSecurityContext(clientContext, security.CommandTLSSettings{}, roachpb.SystemTenantID)
		httpClient, err = secondCtx.GetHTTPClient()
	}
	if err != nil {
		t.Fatalf("Expected success, got %v", err)
	}
	req, err = http.NewRequest("GET", s.AdminURL()+"/_status/metrics/local", nil)
	if err != nil {
		t.Fatalf("could not create request: %v", err)
	}

	resp, err = httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if expected := "certificate signed by unknown authority"; !testutils.IsError(err, expected) {
		t.Fatalf("Expected error %q, got %v", expected, err)
	}

	// Check KV connection.
	if err := db.Put(context.Background(), "foo", "bar"); err != nil {
		t.Error(err)
	}

	// Try with various certificates.
	testCases := []struct {
		user, caName, certPrefix string
		expectedError            string
	}{
		// Certificate signed by wrong client CA.
		{"root", security.EmbeddedCACert, "client.root", "tls: bad certificate"},
		// Success! The node certificate still contains "CN=node" and is signed by ca.crt.
		{"node", security.EmbeddedCACert, "node", "pq: password authentication failed for user node"},
	}

	for i, tc := range testCases {
		pgUrl := makeSecurePGUrl(s.ServingSQLAddr(), tc.user, certsDir, tc.caName, tc.certPrefix+".crt", tc.certPrefix+".key")
		goDB, err := gosql.Open("postgres", pgUrl)
		if err != nil {
			t.Fatal(err)
		}
		defer goDB.Close()

		_, err = goDB.Exec("SELECT 1")
		if !testutils.IsError(err, tc.expectedError) {
			t.Errorf("#%d: expected error %v, got %v", i, tc.expectedError, err)
		}
	}
}

func TestAppendCertificateToBlob(t *testing.T) {
	defer leaktest.AfterTest(t)()

	caBlob, err := securitytest.Asset(filepath.Join(security.EmbeddedCertsDir, security.EmbeddedCACert))
	if err != nil {
		t.Fatal(err)
	}
	testCertPool := x509.NewCertPool()

	certsToAdd := make([][]byte, 0, 3)

	for _, certFilename := range []string{
		//		security.EmbeddedClientCACert,
		//		security.EmbeddedUICACert,
		security.EmbeddedTenantCACert,
	} {
		newCertBlob, err := securitytest.Asset(filepath.Join(security.EmbeddedCertsDir, certFilename))
		if err != nil {
			t.Errorf("failed to read certificate \"%s\": %s", certFilename, err)
			continue
		}

		certsToAdd = append(certsToAdd, bytes.TrimRight(newCertBlob, "\n"))

	}

	caBlob = security.AppendCertificatesToBlob(
		bytes.TrimRight(caBlob, "\n"),
		certsToAdd...,
	)

	if !testCertPool.AppendCertsFromPEM(caBlob) {
		if testing.Verbose() {
			t.Fatalf("appendCertificatesToBlob failed to properly concatenate the test certificates together:\n===\n%s===\n", caBlob)
		} else {
			t.Fatal("appendCertificatesToBlob failed to properly concatenate the test certificates together. Run with the verbose flag set to see the output.")
		}
	}
}
