package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	accountservice "github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	proxyservice "github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	pkgcrypto "github.com/432539/gpt2api/pkg/crypto"
	"github.com/google/uuid"
)

var (
	configPath       = flag.String("c", "configs/config.yaml", "config file path")
	accountID        = flag.Uint64("account-id", 0, "server-side account id")
	proxyID          = flag.Uint64("proxy-id", 0, "enabled runtime proxy id")
	reference        = flag.String("reference-file", "", "optional local reference image")
	requirementsOnly = flag.Bool("requirements-only", false, "stop after one complete requirements exchange")
)

const (
	rootTimeout    = 180 * time.Second
	requestTimeout = 30 * time.Second
	sseWait        = 75 * time.Second
	pollWait       = 60 * time.Second
	maxImageBytes  = 16 * 1024 * 1024
	upstreamModel  = "gpt-5-3"
	acceptanceText = "生成一张白色背景上的蓝色陶瓷马克杯产品照片，画面简洁，无文字"
)

type acceptanceInput struct {
	AuthToken      string            `json:"auth_token"`
	DeviceID       string            `json:"device_id"`
	SessionID      string            `json:"session_id"`
	Cookies        string            `json:"cookies"`
	BrowserProfile string            `json:"browser_profile"`
	Mode           string            `json:"mode,omitempty"`
	PriorResult    *acceptanceResult `json:"prior_result,omitempty"`
	WindowStart    float64           `json:"window_start,omitempty"`
	WindowEnd      float64           `json:"window_end,omitempty"`
}

type phaseResult struct {
	Stage                  string                   `json:"stage"`
	OK                     bool                     `json:"ok"`
	Status                 int                      `json:"status,omitempty"`
	ErrorClass             string                   `json:"error_class,omitempty"`
	ElapsedMS              int64                    `json:"elapsed_ms"`
	Persona                string                   `json:"persona,omitempty"`
	SOTokenPresent         bool                     `json:"so_token_present,omitempty"`
	PollAttempts           int                      `json:"poll_attempts,omitempty"`
	Format                 string                   `json:"format,omitempty"`
	Width                  int                      `json:"width,omitempty"`
	Height                 int                      `json:"height,omitempty"`
	ContentType            string                   `json:"content_type,omitempty"`
	FeatureCount           int                      `json:"feature_count,omitempty"`
	ImageFeatures          []string                 `json:"image_features,omitempty"`
	IsDeactivated          bool                     `json:"is_deactivated,omitempty"`
	CanAccessWithSession   bool                     `json:"can_access_with_session,omitempty"`
	ImageGenerationEnabled bool                     `json:"image_generation_enabled,omitempty"`
	SSETermination         string                   `json:"sse_termination,omitempty"`
	SSEReadFailure         string                   `json:"sse_read_failure,omitempty"`
	FinishType             string                   `json:"finish_type,omitempty"`
	ToolInvoked            bool                     `json:"tool_invoked,omitempty"`
	TurnUseCase            string                   `json:"turn_use_case,omitempty"`
	SawHandoff             bool                     `json:"saw_handoff,omitempty"`
	ConversationIDPresent  bool                     `json:"conversation_id_present,omitempty"`
	ImageTaskIDPresent     bool                     `json:"image_task_id_present,omitempty"`
	AssistantTextPresent   bool                     `json:"assistant_text_present,omitempty"`
	AssistantTextLength    int                      `json:"assistant_text_length,omitempty"`
	Traffic                *chatgpt.TrafficSnapshot `json:"traffic,omitempty"`
}

type acceptanceResult struct {
	OK                 bool                        `json:"ok"`
	IdentityDigest     string                      `json:"identity_digest,omitempty"`
	Phases             []phaseResult               `json:"phases"`
	Traffic            chatgpt.TrafficSnapshot     `json:"traffic"`
	TrafficCheckpoints []chatgpt.TrafficCheckpoint `json:"traffic_checkpoints,omitempty"`
	WindowStart        float64                     `json:"window_start,omitempty"`
	WindowEnd          float64                     `json:"window_end,omitempty"`
}

type acceptanceClient interface {
	GetAccountFeatures(context.Context) (chatgpt.AccountFeatures, error)
	ChatRequirementsV2(context.Context) (*chatgpt.ChatRequirementsResp, error)
	UploadFile(context.Context, []byte, string) (*chatgpt.UploadedFile, error)
	PrepareFConversation(context.Context, chatgpt.ImageConvOpts) (string, error)
	StreamFConversation(context.Context, chatgpt.ImageConvOpts) (<-chan chatgpt.SSEEvent, error)
	PollConversationForImages(context.Context, string, chatgpt.PollOpts) (chatgpt.PollStatus, []string, []string, chatgpt.PollDebug)
	ListRecentConversations(context.Context, int) ([]chatgpt.ConversationSummary, error)
	GetConversationMapping(context.Context, string) (map[string]interface{}, error)
	ImageDownloadURL(context.Context, string, string) (string, error)
	FetchImage(context.Context, string, int64) ([]byte, string, error)
}

func main() {
	flag.Parse()
	input, proxyURL, closeRuntime, err := resolveRuntimeInput(
		context.Background(), *configPath, *accountID, *proxyID, os.Stdin,
	)
	if err != nil {
		writeResult(acceptanceResult{Phases: []phaseResult{{
			Stage: "runtime_resolution", ErrorClass: classifyRuntimeInputError(err),
		}}})
		os.Exit(2)
	}
	defer closeRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), rootTimeout)
	defer cancel()
	meter := &chatgpt.TrafficMeter{}
	client, err := chatgpt.New(optionsForInput(input, proxyURL, meter))
	if err != nil {
		writeResult(acceptanceResult{Phases: []phaseResult{{
			Stage: "client_init", ErrorClass: "invalid_identity",
		}}})
		os.Exit(2)
	}
	referenceImage, err := readAcceptanceReference(*reference)
	if err != nil {
		writeResult(acceptanceResult{Phases: []phaseResult{{
			Stage: "reference_input", ErrorClass: "invalid_reference_file",
		}}})
		os.Exit(2)
	}
	var result acceptanceResult
	if *requirementsOnly {
		result = runRequirementsOnly(ctx, client)
	} else {
		switch input.Mode {
		case "", "image_turn":
			result = runAutomaticAcceptanceWithReference(
				ctx, client, identityDigest(input.SessionID), referenceImage,
			)
		case "resume_existing_image":
			if referenceImage != nil {
				result = acceptanceResult{Phases: []phaseResult{{
					Stage: "input", ErrorClass: "reference_not_allowed_for_resume",
				}}}
				break
			}
			result = resumeAcceptance(
				ctx,
				client,
				input.PriorResult,
				identityDigest(input.SessionID),
				acceptanceWindow{Start: input.WindowStart, End: input.WindowEnd},
			)
		default:
			result = acceptanceResult{Phases: []phaseResult{{
				Stage: "input", ErrorClass: "invalid_mode",
			}}}
		}
	}
	result.IdentityDigest = identityDigest(input.SessionID)
	result.Traffic = meter.Snapshot()
	result.TrafficCheckpoints = meter.Checkpoints()
	writeResult(result)
	if !result.OK {
		os.Exit(1)
	}
}

func runAutomaticAcceptance(
	ctx context.Context,
	client acceptanceClient,
	expectedIdentityDigest string,
) acceptanceResult {
	return runAutomaticAcceptanceWithReference(ctx, client, expectedIdentityDigest, nil)
}

func runAutomaticAcceptanceWithReference(
	ctx context.Context,
	client acceptanceClient,
	expectedIdentityDigest string,
	reference *acceptanceReference,
) acceptanceResult {
	result := runAcceptanceWithReference(ctx, client, reference)
	result.IdentityDigest = expectedIdentityDigest
	window := acceptanceWindow{Start: result.WindowStart, End: result.WindowEnd}
	if validateDownloadResumePrefix(&result, expectedIdentityDigest) != nil ||
		!validAcceptanceWindow(window) {
		return result
	}
	return resumeAcceptance(ctx, client, &result, expectedIdentityDigest, window)
}

type acceptanceReference struct {
	Data     []byte
	FileName string
}

func readAcceptanceReference(path string) (*acceptanceReference, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return nil, errors.New("invalid reference image size")
	}
	return &acceptanceReference{Data: data, FileName: filepath.Base(path)}, nil
}

func readInput(reader io.Reader) (acceptanceInput, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 2*1024*1024))
	decoder.DisallowUnknownFields()
	var input acceptanceInput
	if err := decoder.Decode(&input); err != nil {
		return acceptanceInput{}, errors.New("invalid_json")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return acceptanceInput{}, errors.New("multiple_values")
	}
	return validateInput(input)
}

func validateInput(input acceptanceInput) (acceptanceInput, error) {
	fields := []struct {
		name  string
		value string
	}{
		{"auth_token", input.AuthToken},
		{"device_id", input.DeviceID},
		{"session_id", input.SessionID},
		{"cookies", input.Cookies},
		{"browser_profile", input.BrowserProfile},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return acceptanceInput{}, fmt.Errorf("missing_%s", field.name)
		}
	}
	if input.Mode == "resume_existing_image" && input.PriorResult == nil {
		return acceptanceInput{}, errors.New("missing_prior_result")
	}
	if input.Mode == "resume_existing_image" && !validAcceptanceWindow(acceptanceWindow{
		Start: input.WindowStart,
		End:   input.WindowEnd,
	}) {
		return acceptanceInput{}, errors.New("invalid_acceptance_window")
	}
	return input, nil
}

func classifyRuntimeInputError(err error) string {
	value := err.Error()
	if value == "conflicting_runtime_selectors" {
		return value
	}
	if value == "account_id_required" || strings.HasPrefix(value, "missing_") ||
		value == "invalid_json" || value == "multiple_values" ||
		value == "invalid_acceptance_window" {
		return classifyInputError(err)
	}
	return "invalid_account_runtime"
}

func classifyInputError(err error) string {
	value := err.Error()
	if value == "invalid_json" || value == "multiple_values" || strings.HasPrefix(value, "missing_") {
		return value
	}
	return "invalid_input"
}

type recoveredConversation struct {
	ID  string
	Ref string
}

type acceptanceWindow struct {
	Start float64
	End   float64
}

func validAcceptanceWindow(window acceptanceWindow) bool {
	return window.Start > 0 && window.End >= window.Start && window.End-window.Start <= 60
}

type conversationRecoveryError struct {
	cause error
}

func (errorValue *conversationRecoveryError) Error() string {
	return "conversation_list_failed"
}

func (errorValue *conversationRecoveryError) Unwrap() error {
	if errorValue == nil {
		return nil
	}
	return errorValue.cause
}

func resumeAcceptance(
	ctx context.Context,
	client acceptanceClient,
	prior *acceptanceResult,
	expectedIdentityDigest string,
	window acceptanceWindow,
) acceptanceResult {
	if err := validateDownloadResumePrefix(prior, expectedIdentityDigest); err != nil {
		return acceptanceResult{Phases: []phaseResult{{
			Stage: "resume_validation", ErrorClass: err.Error(),
		}}}
	}
	if !validAcceptanceWindow(window) {
		return acceptanceResult{Phases: []phaseResult{{
			Stage: "resume_validation", ErrorClass: "invalid_acceptance_window",
		}}}
	}

	started := time.Now()
	trafficBefore := acceptanceTrafficSnapshot(client)
	conversation, err := findUniqueExistingAcceptanceConversation(ctx, client, window)
	if err != nil {
		var recoveryError *conversationRecoveryError
		if errors.As(err, &recoveryError) && recoveryError.cause != nil {
			return acceptanceResult{Phases: []phaseResult{
				withAcceptanceTraffic(client, trafficBefore, failedPhase("conversation_recovery", started, recoveryError.cause)),
			}}
		}
		return acceptanceResult{Phases: []phaseResult{withAcceptanceTraffic(client, trafficBefore, phaseResult{
			Stage: "conversation_recovery", ErrorClass: err.Error(), ElapsedMS: elapsedMS(started),
		})}}
	}

	phases := append([]phaseResult(nil), prior.Phases[:4]...)
	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	signedURL, err := client.ImageDownloadURL(ctx, conversation.ID, conversation.Ref)
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("download_url", started, err)))
		return acceptanceResult{Phases: phases}
	}
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
		Stage: "download_url", OK: true, ElapsedMS: elapsedMS(started),
	}))

	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	body, contentType, err := client.FetchImage(ctx, signedURL, maxImageBytes)
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("image_download", started, err)))
		return acceptanceResult{Phases: phases}
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
			Stage: "image_decode", ErrorClass: "invalid_image_bytes", ElapsedMS: elapsedMS(started),
		}))
		return acceptanceResult{Phases: phases}
	}
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
		Stage: "image_decode", OK: true, ElapsedMS: elapsedMS(started),
		Format: format, Width: config.Width, Height: config.Height,
		ContentType: safeContentType(contentType),
	}))

	_, phase, ok := runRequirements(ctx, client, "post_image_requirements")
	phases = append(phases, phase)
	return acceptanceResult{OK: ok, IdentityDigest: expectedIdentityDigest, Phases: phases}
}

func validateDownloadResumePrefix(
	prior *acceptanceResult,
	expectedIdentityDigest string,
) error {
	if prior == nil || prior.OK || prior.IdentityDigest != expectedIdentityDigest {
		return errors.New("identity_mismatch")
	}
	want := []string{
		"account_features",
		"pre_image_requirements",
		"conversation_prepare",
		"image_turn",
		"download_url",
		"image_download",
	}
	if len(prior.Phases) != len(want) {
		return errors.New("invalid_prior_trajectory")
	}
	for index, stage := range want {
		if prior.Phases[index].Stage != stage {
			return errors.New("invalid_prior_trajectory")
		}
	}
	for _, phase := range prior.Phases[:5] {
		if !phase.OK {
			return errors.New("invalid_prior_trajectory")
		}
	}
	features := prior.Phases[0]
	if !features.ImageGenerationEnabled || !features.CanAccessWithSession || features.IsDeactivated {
		return errors.New("invalid_prior_trajectory")
	}
	turn := prior.Phases[3]
	if !turn.ConversationIDPresent || turn.SSETermination != chatgpt.ImageSSETerminationReady {
		return errors.New("invalid_prior_trajectory")
	}
	failedDownload := prior.Phases[5]
	if failedDownload.OK || failedDownload.ErrorClass != "timeout" {
		return errors.New("invalid_prior_trajectory")
	}
	return nil
}

func findUniqueExistingAcceptanceConversation(
	ctx context.Context,
	client acceptanceClient,
	window acceptanceWindow,
) (recoveredConversation, error) {
	summaries, err := client.ListRecentConversations(ctx, 10)
	if err != nil {
		return recoveredConversation{}, &conversationRecoveryError{cause: err}
	}
	matches := make([]recoveredConversation, 0, 1)
	seen := make(map[string]struct{}, len(summaries))
	for _, summary := range summaries {
		if summary.ID == "" {
			continue
		}
		if _, exists := seen[summary.ID]; exists {
			continue
		}
		seen[summary.ID] = struct{}{}
		full, err := client.GetConversationMapping(ctx, summary.ID)
		if err != nil {
			continue
		}
		mapping, _ := full["mapping"].(map[string]interface{})
		if !mappingContainsExactUserTextInWindow(mapping, acceptanceText, window) {
			continue
		}
		refs := imageRefsFromMapping(mapping)
		if len(refs) == 0 {
			continue
		}
		matches = append(matches, recoveredConversation{ID: summary.ID, Ref: refs[0]})
	}
	if len(matches) == 0 {
		return recoveredConversation{}, errors.New("conversation_not_found")
	}
	if len(matches) != 1 {
		return recoveredConversation{}, errors.New("ambiguous_conversation")
	}
	return matches[0], nil
}

func mappingContainsExactUserTextInWindow(
	mapping map[string]interface{},
	expected string,
	window acceptanceWindow,
) bool {
	for _, raw := range mapping {
		node, _ := raw.(map[string]interface{})
		message, _ := node["message"].(map[string]interface{})
		author, _ := message["author"].(map[string]interface{})
		if role, _ := author["role"].(string); role != "user" {
			continue
		}
		content, _ := message["content"].(map[string]interface{})
		createdAt, _ := message["create_time"].(float64)
		if createdAt < window.Start || createdAt > window.End {
			continue
		}
		parts, _ := content["parts"].([]interface{})
		for _, part := range parts {
			if text, ok := part.(string); ok && text == expected {
				return true
			}
		}
	}
	return false
}

func imageRefsFromMapping(mapping map[string]interface{}) []string {
	refs := make([]string, 0, 2)
	seen := make(map[string]struct{})
	for _, message := range chatgpt.ExtractImageToolMsgs(mapping) {
		for _, ref := range message.FileIDs {
			if _, exists := seen[ref]; !exists {
				seen[ref] = struct{}{}
				refs = append(refs, ref)
			}
		}
		for _, ref := range message.SedimentIDs {
			value := "sed:" + ref
			if _, exists := seen[value]; !exists {
				seen[value] = struct{}{}
				refs = append(refs, value)
			}
		}
	}
	return refs
}

func optionsForInput(
	input acceptanceInput,
	proxyURL string,
	meter *chatgpt.TrafficMeter,
) chatgpt.Options {
	return chatgpt.Options{
		AuthToken:      input.AuthToken,
		DeviceID:       input.DeviceID,
		SessionID:      input.SessionID,
		Cookies:        input.Cookies,
		ProxyURL:       proxyURL,
		BrowserProfile: input.BrowserProfile,
		Timeout:        requestTimeout,
		SSETimeout:     sseWait,
		TrafficMeter:   meter,
	}
}

func resolveRuntimeProxy(
	ctx context.Context,
	path string,
	id uint64,
) (string, func(), error) {
	if id == 0 {
		return "", func() {}, errors.New("proxy id required")
	}
	runtime, err := openRuntime(path)
	if err != nil {
		return "", func() {}, err
	}
	proxyRecord, err := runtime.proxies.Get(ctx, id)
	if err != nil || proxyRecord == nil || !proxyRecord.Enabled {
		runtime.close()
		return "", func() {}, errors.New("runtime proxy unavailable")
	}
	proxyURL, err := runtime.proxies.BuildURL(proxyRecord)
	if err != nil || strings.TrimSpace(proxyURL) == "" {
		runtime.close()
		return "", func() {}, errors.New("runtime proxy unavailable")
	}
	return proxyURL, runtime.close, nil
}

type acceptanceRuntime struct {
	accounts *accountservice.Service
	proxies  *proxyservice.Service
	close    func()
}

func openRuntime(path string) (*acceptanceRuntime, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	sqldb, err := db.NewMySQL(cfg.MySQL)
	if err != nil {
		return nil, err
	}
	closeRuntime := func() { _ = sqldb.Close() }
	cipher, err := pkgcrypto.NewAESGCM(cfg.Crypto.AESKey)
	if err != nil {
		closeRuntime()
		return nil, err
	}
	return &acceptanceRuntime{
		accounts: accountservice.NewService(accountservice.NewDAO(sqldb), cipher),
		proxies:  proxyservice.NewService(proxyservice.NewDAO(sqldb), cipher),
		close:    closeRuntime,
	}, nil
}

func resolveRuntimeInput(
	ctx context.Context,
	path string,
	selectedAccountID uint64,
	selectedProxyID uint64,
	reader io.Reader,
) (acceptanceInput, string, func(), error) {
	if selectedAccountID > 0 && selectedProxyID > 0 {
		return acceptanceInput{}, "", func() {}, errors.New("conflicting_runtime_selectors")
	}
	if selectedAccountID == 0 {
		input, err := readInput(reader)
		if err != nil {
			return acceptanceInput{}, "", func() {}, err
		}
		proxyURL, closeRuntime, err := resolveRuntimeProxy(ctx, path, selectedProxyID)
		return input, proxyURL, closeRuntime, err
	}

	runtime, err := openRuntime(path)
	if err != nil {
		return acceptanceInput{}, "", func() {}, err
	}
	fail := func(err error) (acceptanceInput, string, func(), error) {
		runtime.close()
		return acceptanceInput{}, "", func() {}, err
	}
	account, err := runtime.accounts.Get(ctx, selectedAccountID)
	if err != nil || account == nil {
		return fail(errors.New("account runtime unavailable"))
	}
	binding, err := runtime.accounts.GetBinding(ctx, selectedAccountID)
	if err != nil || binding == nil || binding.ProxyID == 0 {
		return fail(errors.New("account proxy unavailable"))
	}
	proxyRecord, err := runtime.proxies.Get(ctx, binding.ProxyID)
	if err != nil || proxyRecord == nil || !proxyRecord.Enabled {
		return fail(errors.New("account proxy unavailable"))
	}
	proxyURL, err := runtime.proxies.BuildURL(proxyRecord)
	if err != nil || strings.TrimSpace(proxyURL) == "" {
		return fail(errors.New("account proxy unavailable"))
	}
	authToken, err := runtime.accounts.DecryptAuthToken(account)
	if err != nil {
		return fail(errors.New("account identity unavailable"))
	}
	cookies, err := runtime.accounts.DecryptCookies(ctx, selectedAccountID)
	if err != nil {
		return fail(errors.New("account identity unavailable"))
	}
	input, err := validateInput(acceptanceInput{
		AuthToken:      authToken,
		DeviceID:       account.OAIDeviceID,
		SessionID:      account.OAISessionID,
		Cookies:        cookies,
		BrowserProfile: account.BrowserProfile,
		Mode:           "image_turn",
	})
	if err != nil {
		return fail(err)
	}
	return input, proxyURL, runtime.close, nil
}

func runAcceptance(ctx context.Context, client acceptanceClient) (result acceptanceResult) {
	return runAcceptanceWithReference(ctx, client, nil)
}

func runRequirementsOnly(ctx context.Context, client acceptanceClient) acceptanceResult {
	_, phase, ok := runRequirements(ctx, client, "pre_image_requirements")
	return acceptanceResult{OK: ok, Phases: []phaseResult{phase}}
}

func runAcceptanceWithReference(
	ctx context.Context,
	client acceptanceClient,
	reference *acceptanceReference,
) (result acceptanceResult) {
	var turnStarted time.Time
	defer func() {
		if turnStarted.IsZero() {
			return
		}
		result.WindowStart = float64(turnStarted.Add(-5*time.Second).UnixMilli()) / 1000
		result.WindowEnd = float64(turnStarted.Add(30*time.Second).UnixMilli()) / 1000
	}()
	phases := make([]phaseResult, 0, 8)
	started := time.Now()
	trafficBefore := acceptanceTrafficSnapshot(client)
	features, err := client.GetAccountFeatures(ctx)
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("account_features", started, err)))
		return acceptanceResult{Phases: phases}
	}
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
		Stage: "account_features", OK: true, ElapsedMS: elapsedMS(started),
		FeatureCount: features.FeatureCount, ImageFeatures: features.ImageFeatures,
		IsDeactivated: features.IsDeactivated, CanAccessWithSession: features.CanAccessWithSession,
		ImageGenerationEnabled: features.ImageGenerationEnabled,
	}))
	if features.IsDeactivated || !features.CanAccessWithSession || !features.ImageGenerationEnabled {
		phases = append(phases, phaseResult{Stage: "image_entitlement", ErrorClass: "image_feature_unavailable"})
		return acceptanceResult{Phases: phases}
	}
	preImage, phase, ok := runRequirements(ctx, client, "pre_image_requirements")
	phases = append(phases, phase)
	if !ok {
		return acceptanceResult{Phases: phases}
	}
	var uploadedReferences []*chatgpt.UploadedFile
	if reference != nil {
		started = time.Now()
		trafficBefore = acceptanceTrafficSnapshot(client)
		uploaded, err := client.UploadFile(ctx, reference.Data, reference.FileName)
		if err != nil {
			phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("reference_upload", started, err)))
			return acceptanceResult{Phases: phases}
		}
		uploadedReferences = append(uploadedReferences, uploaded)
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
			Stage: "reference_upload", OK: true, ElapsedMS: elapsedMS(started),
		}))
	}

	convOpt := chatgpt.ImageConvOpts{
		Prompt:         acceptanceText,
		UpstreamModel:  upstreamModel,
		ParentMsgID:    uuid.NewString(),
		MessageID:      uuid.NewString(),
		ChatToken:      preImage.Token,
		ProofToken:     preImage.ProofToken,
		TurnstileToken: preImage.TurnstileToken,
		SOToken:        preImage.SOToken,
		SSETimeout:     sseWait,
		References:     uploadedReferences,
	}
	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	conduit, err := client.PrepareFConversation(ctx, convOpt)
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("conversation_prepare", started, err)))
		return acceptanceResult{Phases: phases}
	}
	convOpt.ConduitToken = conduit
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: "conversation_prepare", OK: true, ElapsedMS: elapsedMS(started)}))

	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	turnStarted = started
	streamCtx, cancelStream := context.WithCancel(ctx)
	stream, err := client.StreamFConversation(streamCtx, convOpt)
	if err != nil {
		cancelStream()
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("image_turn", started, err)))
		return acceptanceResult{Phases: phases}
	}
	sse := chatgpt.ParseImageSSEUntilReady(stream, chatgpt.ImageSSEParseOpts{
		ExpectedN:          1,
		ReturnWhenPollable: true,
		MaxWait:            sseWait,
	})
	cancelStream()
	turnPhase := imageTurnPhase(sse, started)
	refs := append([]string(nil), sse.FileIDs...)
	for _, ref := range sse.SedimentIDs {
		refs = append(refs, "sed:"+ref)
	}
	if len(refs) == 0 && !sse.ShouldPollForImage() {
		turnPhase.ErrorClass = "image_task_not_started"
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, turnPhase))
		return acceptanceResult{Phases: phases}
	}
	turnPhase.OK = true
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, turnPhase))

	if len(refs) == 0 {
		started = time.Now()
		trafficBefore = acceptanceTrafficSnapshot(client)
		status, fileIDs, sedimentIDs, debug := client.PollConversationForImages(ctx, sse.ConversationID, chatgpt.PollOpts{
			ExpectedN: 1, MaxWait: pollWait, Interval: 3 * time.Second,
			ImageTaskStarted: sse.HasStartedImageTask(),
		})
		if status != chatgpt.PollStatusSuccess {
			phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: "image_poll", ErrorClass: "poll_" + string(status), ElapsedMS: elapsedMS(started), PollAttempts: debug.Attempts}))
			return acceptanceResult{Phases: phases}
		}
		refs = append(refs, fileIDs...)
		for _, ref := range sedimentIDs {
			refs = append(refs, "sed:"+ref)
		}
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: "image_poll", OK: true, ElapsedMS: elapsedMS(started), PollAttempts: debug.Attempts}))
	}
	if len(refs) == 0 {
		phases = append(phases, phaseResult{Stage: "image_reference", ErrorClass: "missing_reference"})
		return acceptanceResult{Phases: phases}
	}

	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	signedURL, err := client.ImageDownloadURL(ctx, sse.ConversationID, refs[0])
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("download_url", started, err)))
		return acceptanceResult{Phases: phases}
	}
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: "download_url", OK: true, ElapsedMS: elapsedMS(started)}))

	started = time.Now()
	trafficBefore = acceptanceTrafficSnapshot(client)
	body, contentType, err := client.FetchImage(ctx, signedURL, maxImageBytes)
	if err != nil {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, failedPhase("image_download", started, err)))
		return acceptanceResult{Phases: phases}
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: "image_decode", ErrorClass: "invalid_image_bytes", ElapsedMS: elapsedMS(started)}))
		return acceptanceResult{Phases: phases}
	}
	phases = append(phases, withAcceptanceTraffic(client, trafficBefore, phaseResult{
		Stage: "image_decode", OK: true, ElapsedMS: elapsedMS(started),
		Format: format, Width: config.Width, Height: config.Height,
		ContentType: safeContentType(contentType),
	}))

	_, phase, ok = runRequirements(ctx, client, "post_image_requirements")
	phases = append(phases, phase)
	return acceptanceResult{OK: ok, Phases: phases}
}

func imageTurnPhase(result chatgpt.ImageSSEResult, started time.Time) phaseResult {
	assistantTextLength := len([]rune(result.AssistantText))
	return phaseResult{
		Stage:                 "image_turn",
		ElapsedMS:             elapsedMS(started),
		SSETermination:        result.Termination,
		SSEReadFailure:        string(result.ReadFailure),
		FinishType:            safeMetadataValue(result.FinishType),
		ToolInvoked:           result.ToolInvoked,
		TurnUseCase:           safeMetadataValue(result.TurnUseCase),
		SawHandoff:            result.SawHandoff,
		ConversationIDPresent: result.ConversationID != "",
		ImageTaskIDPresent:    result.ImageGenTaskID != "",
		AssistantTextPresent:  assistantTextLength > 0,
		AssistantTextLength:   assistantTextLength,
	}
}

func runRequirements(ctx context.Context, client acceptanceClient, stage string) (*chatgpt.ChatRequirementsResp, phaseResult, bool) {
	started := time.Now()
	trafficBefore := acceptanceTrafficSnapshot(client)
	response, err := client.ChatRequirementsV2(ctx)
	if err != nil {
		return nil, withAcceptanceTraffic(client, trafficBefore, failedPhase(stage, started, err)), false
	}
	if strings.TrimSpace(response.Token) == "" {
		return nil, withAcceptanceTraffic(client, trafficBefore, phaseResult{Stage: stage, ErrorClass: "missing_requirements_token", ElapsedMS: elapsedMS(started)}), false
	}
	return response, withAcceptanceTraffic(client, trafficBefore, phaseResult{
		Stage: stage, OK: true, ElapsedMS: elapsedMS(started),
		Persona: safePersona(response.Persona), SOTokenPresent: response.SOToken != "",
	}), true
}

type acceptanceTrafficSnapshotter interface {
	TrafficSnapshot() chatgpt.TrafficSnapshot
}

func acceptanceTrafficSnapshot(client acceptanceClient) *chatgpt.TrafficSnapshot {
	snapshotter, ok := client.(acceptanceTrafficSnapshotter)
	if !ok {
		return nil
	}
	snapshot := snapshotter.TrafficSnapshot()
	return &snapshot
}

func withAcceptanceTraffic(
	client acceptanceClient,
	before *chatgpt.TrafficSnapshot,
	phase phaseResult,
) phaseResult {
	if before == nil {
		return phase
	}
	after := client.(acceptanceTrafficSnapshotter).TrafficSnapshot()
	delta := chatgpt.TrafficSnapshot{
		ProxyReadBytes:          after.ProxyReadBytes - before.ProxyReadBytes,
		ProxyWrittenBytes:       after.ProxyWrittenBytes - before.ProxyWrittenBytes,
		DirectAssetReadBytes:    after.DirectAssetReadBytes - before.DirectAssetReadBytes,
		DirectAssetWrittenBytes: after.DirectAssetWrittenBytes - before.DirectAssetWrittenBytes,
	}
	phase.Traffic = &delta
	return phase
}

func failedPhase(stage string, started time.Time, err error) phaseResult {
	status, class := classifyError(err)
	return phaseResult{Stage: stage, Status: status, ErrorClass: class, ElapsedMS: elapsedMS(started)}
}

func classifyError(err error) (int, string) {
	var upstream *chatgpt.UpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Status {
		case 401:
			return 401, "unauthorized"
		case 403:
			return 403, "forbidden"
		case 429:
			return 429, "rate_limited"
		default:
			return upstream.Status, "upstream_error"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "timeout"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return 0, "transport_error"
	}
	return 0, "local_error"
}

func safePersona(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func safeMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func identityDigest(sessionID string) string {
	digest := sha256.Sum256([]byte("oai-session:" + strings.ToLower(strings.TrimSpace(sessionID))))
	return fmt.Sprintf("%x", digest)
}

func safeContentType(value string) string {
	value = strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
	if !strings.HasPrefix(value, "image/") || len(value) > 64 {
		return ""
	}
	return value
}

func elapsedMS(started time.Time) int64 { return time.Since(started).Milliseconds() }

func writeResult(result acceptanceResult) {
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
