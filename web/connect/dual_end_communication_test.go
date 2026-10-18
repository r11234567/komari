package connectapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/clients"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/enrollment"
	"github.com/komari-monitor/komari/database/metricstore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/web/executionapp"
	browserv1 "github.com/r11234567/komari-proto/gen/go/komari/browser/v1"
	browserv1connect "github.com/r11234567/komari-proto/gen/go/komari/browser/v1/browserv1connect"
	enrollmentv1 "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1"
	enrollmentv1connect "github.com/r11234567/komari-proto/gen/go/komari/enrollment/v1/enrollmentv1connect"
	execv1 "github.com/r11234567/komari-proto/gen/go/komari/exec/v1"
	execv1connect "github.com/r11234567/komari-proto/gen/go/komari/exec/v1/execv1connect"
	metricsv1 "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1"
	metricsv1connect "github.com/r11234567/komari-proto/gen/go/komari/metrics/v1/metricsv1connect"
	reportv1 "github.com/r11234567/komari-proto/gen/go/komari/report/v1"
	reportv1connect "github.com/r11234567/komari-proto/gen/go/komari/report/v1/reportv1connect"
	securityv1 "github.com/r11234567/komari-proto/gen/go/komari/security/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var (
	testServer       *httptest.Server
	testClientUUID   string
	testAccessToken  string
	testAgentPubKey  ed25519.PublicKey
	testAgentPrivKey ed25519.PrivateKey
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)

	tempDir, err := os.MkdirTemp("", "komari-dualend-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "test_komari.db")
	flags.DatabaseFile = dbPath
	flags.DatabaseType = flags.DatabaseTypeSQLite

	if err := dbcore.Initialize(); err != nil {
		panic(err)
	}

	metricsPath := filepath.Join(tempDir, "test_metrics.db")
	_ = os.Setenv("METRICS_SQLITE_FILE", metricsPath)
	_ = metricstore.InitializeStore()

	uuid, _, err := clients.CreateClient()
	if err != nil {
		panic(err)
	}
	testClientUUID = uuid

	// Provision active OAuth/Device access token credentials for agent
	testAccessToken = "test-access-token-1234567890abcdef"
	hash := sha256.Sum256([]byte(testAccessToken))
	accessHash := hex.EncodeToString(hash[:])

	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	testAgentPubKey = pubKey
	testAgentPrivKey = privKey

	now := time.Now().UTC()
	cred := models.AgentCredential{
		Client:           testClientUUID,
		AccessTokenHash:  accessHash,
		AccessExpiresAt:  now.Add(24 * time.Hour),
		RefreshTokenHash: accessHash,
		RefreshExpiresAt: now.Add(180 * 24 * time.Hour),
		AgentPublicKey:   base64.StdEncoding.EncodeToString(testAgentPubKey),
		AgentKeyID:       "key-1",
		KeyAlgorithm:     1,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := dbcore.GetDBInstance().Create(&cred).Error; err != nil {
		panic(err)
	}

	r := gin.New()
	Register(r)

	testServer = httptest.NewServer(r)

	code := m.Run()

	testServer.Close()
	_ = metricstore.CloseStoreContext(context.Background())
	_ = dbcore.Close()

	os.Exit(code)
}

// TestDualEnd_PublicBrowserRPC tests unauthenticated client-to-server RPC over HTTP.
func TestDualEnd_PublicBrowserRPC(t *testing.T) {
	client := browserv1connect.NewBrowserServiceClient(testServer.Client(), testServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.GetPublicInfo(ctx, connect.NewRequest(&browserv1.GetPublicInfoRequest{}))
	if err != nil {
		t.Fatalf("GetPublicInfo failed across dual-end network: %v", err)
	}

	if resp.Msg == nil {
		t.Fatal("expected non-nil GetPublicInfoResponse")
	}
	t.Logf("Dual-end GetPublicInfo succeeded: SiteName=%q, Version=%q", resp.Msg.SiteName, resp.Msg.Version)
}

// TestDualEnd_PermissionEnforcement verifies the network interceptor blocks unauthenticated access to agent endpoints.
func TestDualEnd_PermissionEnforcement(t *testing.T) {
	reportClient := reportv1connect.NewAgentReportServiceClient(testServer.Client(), testServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := connect.NewRequest(&reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
		},
	})
	// Intentionally no Authorization header

	_, err := reportClient.SubmitReport(ctx, req)
	if err == nil {
		t.Fatal("expected PermissionDenied for unauthenticated SubmitReport, got nil error")
	}

	if connect.CodeOf(err) != connect.CodePermissionDenied && connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected CodePermissionDenied, got: %v (code: %v)", err, connect.CodeOf(err))
	}
	t.Logf("Dual-end interceptor correctly rejected unauthenticated report: %v", err)
}

// TestDualEnd_AuthenticatedAgentReport tests real agent report transmission over HTTP with Bearer token.
func TestDualEnd_AuthenticatedAgentReport(t *testing.T) {
	reportClient := reportv1connect.NewAgentReportServiceClient(testServer.Client(), testServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := connect.NewRequest(&reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System: &reportv1.SystemInfo{
				Hostname:         "dual-end-test-host",
				CpuName:          "AMD EPYC 9654",
				CpuCount:         64,
				Architecture:     "amd64",
				Os:               "linux",
				KernelVersion:    "6.8.0-generic",
				Virtualization:   "kvm",
				MemoryTotalBytes: 64 << 30,
			},
			Resources: &reportv1.ResourceUsage{
				MemoryUsedBytes: 8 << 30,
				SwapTotalBytes:  4 << 30,
				SwapUsedBytes:   0,
			},
			Metadata: &reportv1.AgentMetadata{
				Version: "v1.2.35",
			},
		},
	})
	req.Header().Set("Authorization", "Bearer "+testAccessToken)

	resp, err := reportClient.SubmitReport(ctx, req)
	if err != nil {
		t.Fatalf("SubmitReport failed over dual-end network: %v", err)
	}

	if resp == nil {
		t.Fatal("expected non-nil SubmitReportResponse")
	}
	t.Logf("Dual-end SubmitReport succeeded: acknowledged by server successfully")
}

// TestDualEnd_AuthenticatedMetricsSubmit tests real metric points ingestion over HTTP.
func TestDualEnd_AuthenticatedMetricsSubmit(t *testing.T) {
	metricsClient := metricsv1connect.NewMetricsServiceClient(testServer.Client(), testServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	points := completeMetricPoints()
	req := connect.NewRequest(&metricsv1.SubmitMetricsRequest{
		AgentId:  testClientUUID,
		Sequence: 1,
		Points:   points,
	})
	req.Header().Set("Authorization", "Bearer "+testAccessToken)

	resp, err := metricsClient.SubmitMetrics(ctx, req)
	if err != nil {
		t.Fatalf("SubmitMetrics failed over dual-end network: %v", err)
	}

	if resp.Msg.AcceptedSequence != 1 || resp.Msg.AcceptedPoints != uint32(len(points)) {
		t.Fatalf("unexpected response: sequence=%d, points=%d", resp.Msg.AcceptedSequence, resp.Msg.AcceptedPoints)
	}
	t.Logf("Dual-end SubmitMetrics succeeded: sequence=%d, accepted_points=%d", resp.Msg.AcceptedSequence, resp.Msg.AcceptedPoints)
}

// TestDualEnd_EnrollmentHandshake tests unauthenticated device enrollment handshake over HTTP.
func TestDualEnd_EnrollmentHandshake(t *testing.T) {
	enrollClient := enrollmentv1connect.NewEnrollmentServiceClient(testServer.Client(), testServer.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := connect.NewRequest(&enrollmentv1.BeginEnrollmentRequest{
		AgentPublicKey: &securityv1.PublicKey{
			Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519,
			Value:     []byte("01234567890123456789012345678901"),
			KeyId:     "agent-test-key-01",
		},
		Device: &enrollmentv1.DeviceIdentity{
			Hostname:        "new-node-01",
			Architecture:    "amd64",
			OperatingSystem: "linux",
			AgentVersion:    "v1.0.0",
		},
	})

	resp, err := enrollClient.BeginEnrollment(ctx, req)
	if err != nil {
		t.Fatalf("BeginEnrollment failed over dual-end network: %v", err)
	}

	if resp.Msg.DeviceCode == "" || resp.Msg.UserCode == "" {
		t.Fatalf("expected device_code and user_code, got: %+v", resp.Msg)
	}
	t.Logf("Dual-end BeginEnrollment succeeded: DeviceCode=%s, UserCode=%s, VerificationUri=%s",
		resp.Msg.DeviceCode, resp.Msg.UserCode, resp.Msg.VerificationUri)
}

func buildSignedDPoPHeaders(method, path, token string, body []byte, privKey ed25519.PrivateKey, nonce string, ts int64) map[string]string {
	tokenDigest := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(tokenDigest[:])

	bodyDigest := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(bodyDigest[:])

	tsStr := strconv.FormatInt(ts, 10)
	payload := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s", method, path, tsStr, nonce, tokenHash, bodyHash)

	sig := ed25519.Sign(privKey, []byte(payload))

	return map[string]string{
		"Authorization":      "Bearer " + token,
		"X-Komari-Signature": base64.StdEncoding.EncodeToString(sig),
		"X-Komari-Timestamp": tsStr,
		"X-Komari-Nonce":     nonce,
	}
}

// TestDualEnd_DPoP_ValidSignatureWithBodyHash tests an agent request protected with RFC 9449 DPoP + RequestBodyHash
func TestDualEnd_DPoP_ValidSignatureWithBodyHash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	submitReq := &reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System: &reportv1.SystemInfo{
				Hostname: "dpop-node-01",
			},
		},
	}
	bodyBytes, err := proto.Marshal(submitReq)
	if err != nil {
		t.Fatal(err)
	}

	path := "/komari.report.v1.AgentReportService/SubmitReport"
	headers := buildSignedDPoPHeaders("POST", path, testAccessToken, bodyBytes, testAgentPrivKey, uuid.NewString(), time.Now().Unix())

	httpReq, err := http.NewRequestWithContext(ctx, "POST", testServer.URL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := testServer.Client().Do(httpReq)
	if err != nil {
		t.Fatalf("DPoP request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}
	t.Log("DPoP valid signature with body hash accepted successfully")
}

// TestDualEnd_DPoP_TamperedBodyRejected tests that tampering with the request body causes DPoP verification to reject with 401
func TestDualEnd_DPoP_TamperedBodyRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	submitReq := &reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System:  &reportv1.SystemInfo{Hostname: "original-host"},
		},
	}
	originalBytes, _ := proto.Marshal(submitReq)

	tamperedReq := &reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System:  &reportv1.SystemInfo{Hostname: "tampered-host"},
		},
	}
	tamperedBytes, _ := proto.Marshal(tamperedReq)

	path := "/komari.report.v1.AgentReportService/SubmitReport"
	// Sign with originalBytes
	headers := buildSignedDPoPHeaders("POST", path, testAccessToken, originalBytes, testAgentPrivKey, uuid.NewString(), time.Now().Unix())

	// Send tamperedBytes with signature of originalBytes
	httpReq, err := http.NewRequestWithContext(ctx, "POST", testServer.URL+path, bytes.NewReader(tamperedBytes))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := testServer.Client().Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for tampered body, got %d", resp.StatusCode)
	}
	t.Log("DPoP correctly rejected body tampering in transit")
}

// TestDualEnd_DPoP_ReplayNonceRejected tests that replaying the exact same nonce is rejected
func TestDualEnd_DPoP_ReplayNonceRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	submitReq := &reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System:  &reportv1.SystemInfo{Hostname: "replay-host"},
		},
	}
	bodyBytes, _ := proto.Marshal(submitReq)

	path := "/komari.report.v1.AgentReportService/SubmitReport"
	replayNonce := "fixed-replay-nonce-" + uuid.NewString()
	headers := buildSignedDPoPHeaders("POST", path, testAccessToken, bodyBytes, testAgentPrivKey, replayNonce, time.Now().Unix())

	sendReq := func() int {
		httpReq, _ := http.NewRequestWithContext(ctx, "POST", testServer.URL+path, bytes.NewReader(bodyBytes))
		httpReq.Header.Set("Content-Type", "application/proto")
		for k, v := range headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := testServer.Client().Do(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	firstCode := sendReq()
	if firstCode != http.StatusOK {
		t.Fatalf("first request expected 200 OK, got %d", firstCode)
	}

	secondCode := sendReq()
	if secondCode != http.StatusUnauthorized {
		t.Fatalf("second replayed request expected 401 Unauthorized, got %d", secondCode)
	}
	t.Log("DPoP anti-replay correctly rejected reused nonce")
}

// TestDualEnd_DPoP_DriftedTimestampRejected tests that requests with timestamp drift > 120s are rejected
func TestDualEnd_DPoP_DriftedTimestampRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	submitReq := &reportv1.SubmitReportRequest{
		Report: &reportv1.AgentReport{
			AgentId: testClientUUID,
			System:  &reportv1.SystemInfo{Hostname: "drift-host"},
		},
	}
	bodyBytes, _ := proto.Marshal(submitReq)

	path := "/komari.report.v1.AgentReportService/SubmitReport"
	headers := buildSignedDPoPHeaders("POST", path, testAccessToken, bodyBytes, testAgentPrivKey, uuid.NewString(), time.Now().Unix()-300)

	httpReq, _ := http.NewRequestWithContext(ctx, "POST", testServer.URL+path, bytes.NewReader(bodyBytes))
	httpReq.Header.Set("Content-Type", "application/proto")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := testServer.Client().Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for drifted timestamp, got %d", resp.StatusCode)
	}
	t.Log("DPoP correctly rejected drifted timestamp")
}

// TestDualEnd_SingleUseDeviceCode tests that an approved device code is atomically consumed on first retrieval
func TestDualEnd_SingleUseDeviceCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	enrollClient := enrollmentv1connect.NewEnrollmentServiceClient(testServer.Client(), testServer.URL)

	// 1. Begin
	beginResp, err := enrollClient.BeginEnrollment(ctx, connect.NewRequest(&enrollmentv1.BeginEnrollmentRequest{
		AgentPublicKey: &securityv1.PublicKey{
			Algorithm: securityv1.SignatureAlgorithm_SIGNATURE_ALGORITHM_ED25519,
			Value:     testAgentPubKey,
			KeyId:     "single-use-key",
		},
		Device: &enrollmentv1.DeviceIdentity{
			Hostname:        "single-use-node",
			OperatingSystem: "linux",
			Architecture:    "amd64",
		},
	}))
	if err != nil {
		t.Fatalf("BeginEnrollment failed: %v", err)
	}
	devCode := beginResp.Msg.GetDeviceCode()
	userCode := beginResp.Msg.GetUserCode()

	// 2. Approve for a dedicated client so testClientUUID credentials remain intact
	singleUseUUID, _, err := clients.CreateClient()
	if err != nil {
		t.Fatalf("CreateClient failed: %v", err)
	}
	_, err = enrollment.Approve(userCode, singleUseUUID, "admin")
	if err != nil {
		t.Fatalf("Approve failed: %v", err)
	}

	// 3. First Poll: returns APPROVED and issued credentials
	poll1, err := enrollClient.PollEnrollment(ctx, connect.NewRequest(&enrollmentv1.PollEnrollmentRequest{
		DeviceCode: devCode,
	}))
	if err != nil {
		t.Fatalf("Poll 1 failed: %v", err)
	}
	if poll1.Msg.GetState() != enrollmentv1.EnrollmentState_ENROLLMENT_STATE_APPROVED {
		t.Fatalf("expected APPROVED, got %v", poll1.Msg.GetState())
	}
	if poll1.Msg.GetCredentials() == nil || poll1.Msg.GetCredentials().GetAccessToken() == "" {
		t.Fatal("expected credentials in first poll")
	}

	// 4. Second Poll: immediately returns EXPIRED because device code was atomically consumed
	poll2, err := enrollClient.PollEnrollment(ctx, connect.NewRequest(&enrollmentv1.PollEnrollmentRequest{
		DeviceCode: devCode,
	}))
	if err != nil {
		t.Fatalf("Poll 2 failed: %v", err)
	}
	if poll2.Msg.GetState() != enrollmentv1.EnrollmentState_ENROLLMENT_STATE_EXPIRED {
		t.Fatalf("expected EXPIRED on reused device code, got %v", poll2.Msg.GetState())
	}
	t.Log("Single-use device code verified: successfully consumed on first use and blocked on replay")
}

// TestDualEnd_Execution_SignedEnvelopeLease tests command leasing with Ed25519 SignedEnvelope
func TestDualEnd_Execution_SignedEnvelopeLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create batch execution
	executions, err := executionapp.CreateBatch([]string{testClientUUID}, "admin-1", "idem-"+uuid.NewString(), &execv1.ExecutionSpec{
		Command: "uname -a",
		Timeout: durationpb.New(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateBatch failed: %v", err)
	}
	if len(executions) == 0 {
		t.Fatal("expected non-empty executions")
	}

	execClient := execv1connect.NewExecutionServiceClient(testServer.Client(), testServer.URL)
	leaseReq := connect.NewRequest(&execv1.LeaseExecutionRequest{AgentId: testClientUUID})
	leaseReq.Header().Set("Authorization", "Bearer "+testAccessToken)

	stream, err := execClient.LeaseExecution(ctx, leaseReq)
	if err != nil {
		t.Fatalf("LeaseExecution failed: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected stream message, got err: %v", stream.Err())
	}

	msg := stream.Msg()
	assign := msg.GetAssignment()
	if assign == nil {
		t.Fatal("expected non-nil assignment")
	}

	envelope := assign.GetEnvelope()
	if envelope == nil {
		t.Fatal("expected non-nil SignedEnvelope in ExecutionAssignment")
	}
	if len(envelope.GetSignatures()) == 0 {
		t.Fatal("expected Ed25519 signature in envelope")
	}

	// Unpack SignedInstruction from envelope payload
	var inst securityv1.SignedInstruction
	if err := proto.Unmarshal(envelope.GetPayload(), &inst); err != nil {
		t.Fatalf("failed to unmarshal SignedInstruction from envelope: %v", err)
	}
	if inst.GetAgentId() != testClientUUID {
		t.Fatalf("agent ID mismatch: expected %q, got %q", testClientUUID, inst.GetAgentId())
	}
	if inst.GetInstructionType() != "exec:v1" {
		t.Fatalf("instruction type mismatch: %q", inst.GetInstructionType())
	}

	// Unpack ExecutionSpec from instruction body
	var spec execv1.ExecutionSpec
	if err := proto.Unmarshal(inst.GetBody(), &spec); err != nil {
		t.Fatalf("failed to unmarshal ExecutionSpec from instruction body: %v", err)
	}
	if spec.GetCommand() != "uname -a" {
		t.Fatalf("command mismatch: %q", spec.GetCommand())
	}
	t.Log("Execution SignedEnvelope leasing and cryptographic unpacking verified successfully")
}

