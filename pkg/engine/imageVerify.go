package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	"github.com/kyverno/kyverno/pkg/autogen"
	"github.com/kyverno/kyverno/pkg/cosign"
	enginecontext "github.com/kyverno/kyverno/pkg/engine/context"
	"github.com/kyverno/kyverno/pkg/engine/response"
	"github.com/kyverno/kyverno/pkg/engine/variables"
	"github.com/kyverno/kyverno/pkg/logging"
	"github.com/kyverno/kyverno/pkg/registryclient"
	"github.com/kyverno/kyverno/pkg/tracing"
	apiutils "github.com/kyverno/kyverno/pkg/utils/api"
	"github.com/kyverno/kyverno/pkg/utils/jsonpointer"
	"github.com/kyverno/kyverno/pkg/utils/wildcard"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func getMatchingImages(images map[string]map[string]apiutils.ImageInfo, rule *kyvernov1.Rule) ([]apiutils.ImageInfo, string) {
	imageInfos := []apiutils.ImageInfo{}
	imageRefs := []string{}
	for _, infoMap := range images {
		for _, imageInfo := range infoMap {
			image := imageInfo.String()
			for _, verifyImage := range rule.VerifyImages {
				verifyImage = *verifyImage.Convert()
				imageRefs = append(imageRefs, verifyImage.ImageReferences...)
				if imageMatches(image, verifyImage.ImageReferences) {
					imageInfos = append(imageInfos, imageInfo)
				}
			}
		}
	}
	return imageInfos, strings.Join(imageRefs, ",")
}

func extractMatchingImages(policyContext *PolicyContext, rule *kyvernov1.Rule) ([]apiutils.ImageInfo, string, error) {
	var (
		images map[string]map[string]apiutils.ImageInfo
		err    error
	)
	images = policyContext.jsonContext.ImageInfo()
	if rule.ImageExtractors != nil {
		images, err = policyContext.jsonContext.GenerateCustomImageInfo(
			&policyContext.newResource, rule.ImageExtractors)
		if err != nil {
			// if we get an error while generating custom images from image extractors,
			// don't check for matching images in imageExtractors
			return nil, "", err
		}
	}
	matchingImages, imageRefs := getMatchingImages(images, rule)
	return matchingImages, imageRefs, nil
}

func VerifyAndPatchImages(
	ctx context.Context,
	rclient registryclient.Client,
	policyContext *PolicyContext,
) (*response.EngineResponse, *ImageVerificationMetadata) {
	resp := &response.EngineResponse{}

	policy := policyContext.policy
	patchedResource := policyContext.newResource
	logger := logging.WithName("EngineVerifyImages").WithValues("policy", policy.GetName(),
		"kind", patchedResource.GetKind(), "namespace", patchedResource.GetNamespace(), "name", patchedResource.GetName())

	startTime := time.Now()
	defer func() {
		buildResponse(policyContext, resp, startTime)
		logger.V(4).Info("processed image verification rules",
			"time", resp.PolicyResponse.ProcessingTime.String(),
			"applied", resp.PolicyResponse.RulesAppliedCount, "successful", resp.IsSuccessful())
	}()

	policyContext.jsonContext.Checkpoint()
	defer policyContext.jsonContext.Restore()

	ivm := &ImageVerificationMetadata{}
	rules := autogen.ComputeRules(policyContext.policy)
	applyRules := policy.GetSpec().GetApplyRules()

	for i := range rules {
		rule := &rules[i]

		tracing.ChildSpan(
			ctx,
			"pkg/engine",
			fmt.Sprintf("RULE %s", rule.Name),
			func(ctx context.Context, span trace.Span) {
				if len(rule.VerifyImages) == 0 {
					return
				}

				if !matches(logger, rule, policyContext) {
					return
				}

				logger.V(3).Info("processing image verification rule", "ruleSelector", applyRules)

				var err error
				ruleImages, imageRefs, err := extractMatchingImages(policyContext, rule)
				if err != nil {
					appendResponse(resp, rule, fmt.Sprintf("failed to extract images: %s", err.Error()), response.RuleStatusError)
					return
				}
				if len(ruleImages) == 0 {
					appendResponse(
						resp,
						rule,
						fmt.Sprintf("skip run verification as image in resource not found in imageRefs '%s'", imageRefs),
						response.RuleStatusSkip,
					)
					return
				}

				policyContext.jsonContext.Restore()
				if err := LoadContext(ctx, logger, rclient, rule.Context, policyContext, rule.Name); err != nil {
					appendResponse(resp, rule, fmt.Sprintf("failed to load context: %s", err.Error()), response.RuleStatusError)
					return
				}

				ruleCopy, err := substituteVariables(rule, policyContext.jsonContext, logger)
				if err != nil {
					appendResponse(resp, rule, fmt.Sprintf("failed to substitute variables: %s", err.Error()), response.RuleStatusError)
					return
				}

				iv := &imageVerifier{
					logger:        logger,
					rclient:       rclient,
					policyContext: policyContext,
					rule:          ruleCopy,
					resp:          resp,
					ivm:           ivm,
				}

				for _, imageVerify := range ruleCopy.VerifyImages {
					iv.verify(ctx, imageVerify, ruleImages)
				}
			},
		)

		if applyRules == kyvernov1.ApplyOne && resp.PolicyResponse.RulesAppliedCount > 0 {
			break
		}
	}

	return resp, ivm
}

func appendResponse(resp *response.EngineResponse, rule *kyvernov1.Rule, msg string, status response.RuleStatus) {
	rr := ruleResponse(*rule, response.ImageVerify, msg, status)
	resp.PolicyResponse.Rules = append(resp.PolicyResponse.Rules, *rr)
	incrementErrorCount(resp)
}

func substituteVariables(rule *kyvernov1.Rule, ctx enginecontext.EvalInterface, logger logr.Logger) (*kyvernov1.Rule, error) {
	// remove attestations as variables are not substituted in them
	ruleCopy := *rule.DeepCopy()
	for i := range ruleCopy.VerifyImages {
		ruleCopy.VerifyImages[i].Attestations = nil
	}

	var err error
	ruleCopy, err = variables.SubstituteAllInRule(logger, ctx, ruleCopy)
	if err != nil {
		return nil, err
	}

	// replace attestations
	for i := range rule.VerifyImages {
		ruleCopy.VerifyImages[i].Attestations = rule.VerifyImages[i].Attestations
	}

	return &ruleCopy, nil
}

type imageVerifier struct {
	logger        logr.Logger
	rclient       registryclient.Client
	policyContext *PolicyContext
	rule          *kyvernov1.Rule
	resp          *response.EngineResponse
	ivm           *ImageVerificationMetadata
}

// verify applies policy rules to each matching image. The policy rule results and annotation patches are
// added to tme imageVerifier `resp` and `ivm` fields.
func (iv *imageVerifier) verify(ctx context.Context, imageVerify kyvernov1.ImageVerification, matchedImageInfos []apiutils.ImageInfo) {
	// for backward compatibility
	imageVerify = *imageVerify.Convert()

	for _, imageInfo := range matchedImageInfos {
		image := imageInfo.String()

		if hasImageVerifiedAnnotationChanged(iv.policyContext, iv.logger) {
			msg := imageVerifyAnnotationKey + " annotation cannot be changed"
			iv.logger.Info("image verification error", "reason", msg)
			ruleResp := ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusFail)
			iv.resp.PolicyResponse.Rules = append(iv.resp.PolicyResponse.Rules, *ruleResp)
			incrementAppliedCount(iv.resp)
			continue
		}

		pointer := jsonpointer.ParsePath(imageInfo.Pointer).JMESPath()
		changed, err := iv.policyContext.jsonContext.HasChanged(pointer)
		if err == nil && !changed {
			iv.logger.V(4).Info("no change in image, skipping check", "image", image)
			continue
		}

		verified, err := isImageVerified(iv.policyContext.newResource, image, iv.logger)
		if err == nil && verified {
			iv.logger.Info("image was previously verified, skipping check", "image", image)
			continue
		}

		ruleResp, digest := iv.verifyImage(ctx, imageVerify, imageInfo)

		if imageVerify.MutateDigest {
			patch, retrievedDigest, err := iv.handleMutateDigest(ctx, digest, imageInfo)
			if err != nil {
				ruleResp = ruleError(iv.rule, response.ImageVerify, "failed to update digest", err)
			} else if patch != nil {
				if ruleResp == nil {
					ruleResp = ruleResponse(*iv.rule, response.ImageVerify, "mutated image digest", response.RuleStatusPass)
				}

				ruleResp.Patches = append(ruleResp.Patches, patch)
				imageInfo.Digest = retrievedDigest
				image = imageInfo.String()
			}
		}

		if ruleResp != nil {
			if len(imageVerify.Attestors) > 0 || len(imageVerify.Attestations) > 0 {
				verified := ruleResp.Status == response.RuleStatusPass
				iv.ivm.add(image, verified)
			}

			iv.resp.PolicyResponse.Rules = append(iv.resp.PolicyResponse.Rules, *ruleResp)
			incrementAppliedCount(iv.resp)
		}
	}
}

func (iv *imageVerifier) handleMutateDigest(ctx context.Context, digest string, imageInfo apiutils.ImageInfo) ([]byte, string, error) {
	if imageInfo.Digest != "" {
		return nil, "", nil
	}

	if digest == "" {
		desc, err := iv.rclient.FetchImageDescriptor(ctx, imageInfo.String())
		if err != nil {
			return nil, "", err
		}
		digest = desc.Digest.String()
	}

	patch, err := makeAddDigestPatch(imageInfo, digest)
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to create image digest patch")
	}

	iv.logger.V(4).Info("adding digest patch", "image", imageInfo.String(), "patch", string(patch))

	return patch, digest, nil
}

func hasImageVerifiedAnnotationChanged(ctx *PolicyContext, log logr.Logger) bool {
	if reflect.DeepEqual(ctx.newResource, unstructured.Unstructured{}) ||
		reflect.DeepEqual(ctx.oldResource, unstructured.Unstructured{}) {
		return false
	}

	key := imageVerifyAnnotationKey
	newValue := ctx.newResource.GetAnnotations()[key]
	oldValue := ctx.oldResource.GetAnnotations()[key]
	result := newValue != oldValue
	if result {
		log.V(2).Info("annotation mismatch", "oldValue", oldValue, "newValue", newValue, "key", key)
	}

	return result
}

func imageMatches(image string, imagePatterns []string) bool {
	for _, imagePattern := range imagePatterns {
		if wildcard.Match(imagePattern, image) {
			return true
		}
	}

	return false
}

func (iv *imageVerifier) verifyImage(
	ctx context.Context,
	imageVerify kyvernov1.ImageVerification,
	imageInfo apiutils.ImageInfo,
) (*response.RuleResponse, string) {
	if len(imageVerify.Attestors) <= 0 && len(imageVerify.Attestations) <= 0 {
		return nil, ""
	}

	image := imageInfo.String()
	iv.logger.V(2).Info("verifying image signatures", "image", image,
		"attestors", len(imageVerify.Attestors), "attestations", len(imageVerify.Attestations))

	if err := iv.policyContext.jsonContext.AddImageInfo(imageInfo); err != nil {
		iv.logger.Error(err, "failed to add image to context")
		msg := fmt.Sprintf("failed to add image to context %s: %s", image, err.Error())
		return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusError), ""
	}

	if len(imageVerify.Attestors) > 0 {
		ruleResp, cosignResp := iv.verifyAttestors(ctx, imageVerify.Attestors, imageVerify, imageInfo, "")
		if ruleResp.Status != response.RuleStatusPass {
			return ruleResp, ""
		}

		if len(imageVerify.Attestations) == 0 {
			return ruleResp, cosignResp.Digest
		}

		if imageInfo.Digest == "" {
			imageInfo.Digest = cosignResp.Digest
		}

		if len(imageVerify.Attestations) == 0 {
			return ruleResp, cosignResp.Digest
		}

		if imageInfo.Digest == "" {
			imageInfo.Digest = cosignResp.Digest
		}
	}

	return iv.verifyAttestations(ctx, imageVerify, imageInfo)
}

func (iv *imageVerifier) verifyAttestors(
	ctx context.Context,
	attestors []kyvernov1.AttestorSet,
	imageVerify kyvernov1.ImageVerification,
	imageInfo apiutils.ImageInfo,
	predicateType string,
) (*response.RuleResponse, *cosign.Response) {
	var cosignResponse *cosign.Response
	image := imageInfo.String()

	for i, attestorSet := range attestors {
		var err error
		path := fmt.Sprintf(".attestors[%d]", i)
		iv.logger.V(4).Info("verifying attestors", "path", path)
		cosignResponse, err = iv.verifyAttestorSet(ctx, attestorSet, imageVerify, imageInfo, path)
		if err != nil {
			iv.logger.Error(err, "failed to verify image")
			return iv.handleRegistryErrors(image, err), nil
		}
	}

	if cosignResponse == nil {
		return ruleError(iv.rule, response.ImageVerify, "invalid response", fmt.Errorf("nil")), nil
	}

	msg := fmt.Sprintf("verified image signatures for %s", image)
	return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusPass), cosignResponse
}

// handle registry network errors as a rule error (instead of a policy failure)
func (iv *imageVerifier) handleRegistryErrors(image string, err error) *response.RuleResponse {
	msg := fmt.Sprintf("failed to verify image %s: %s", image, err.Error())
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusError)
	}

	return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusFail)
}

func (iv *imageVerifier) verifyAttestations(
	ctx context.Context,
	imageVerify kyvernov1.ImageVerification,
	imageInfo apiutils.ImageInfo,
) (*response.RuleResponse, string) {
	image := imageInfo.String()
	for i, attestation := range imageVerify.Attestations {
		var attestationError error
		path := fmt.Sprintf(".attestations[%d]", i)

		if attestation.PredicateType == "" {
			return ruleResponse(*iv.rule, response.ImageVerify, path+": missing predicateType", response.RuleStatusFail), ""
		}

		if len(attestation.Attestors) == 0 {
			// add an empty attestor to allow fetching and checking attestations
			attestation.Attestors = []kyvernov1.AttestorSet{{Entries: []kyvernov1.Attestor{{}}}}
		}

		for j, attestor := range attestation.Attestors {
			attestorPath := fmt.Sprintf("%s.attestors[%d]", path, j)
			requiredCount := getRequiredCount(attestor)
			verifiedCount := 0

			for _, a := range attestor.Entries {
				entryPath := fmt.Sprintf("%s.entries[%d]", attestorPath, i)
				opts, subPath := iv.buildOptionsAndPath(a, imageVerify, image, &imageVerify.Attestations[i])
				cosignResp, err := cosign.FetchAttestations(ctx, iv.rclient, *opts)
				if err != nil {
					iv.logger.Error(err, "failed to fetch attestations")
					return iv.handleRegistryErrors(image, err), ""
				}

				if imageInfo.Digest == "" {
					imageInfo.Digest = cosignResp.Digest
					image = imageInfo.String()
				}

				attestationError = iv.verifyAttestation(cosignResp.Statements, attestation, imageInfo)
				if attestationError != nil {
					attestationError = errors.Wrapf(attestationError, entryPath+subPath)
					return ruleResponse(*iv.rule, response.ImageVerify, attestationError.Error(), response.RuleStatusFail), ""
				}

				verifiedCount++
				if verifiedCount >= requiredCount {
					iv.logger.V(2).Info("image attestations verification succeeded", "verifiedCount", verifiedCount, "requiredCount", requiredCount)
					break
				}
			}

			if verifiedCount < requiredCount {
				msg := fmt.Sprintf("image attestations verification failed, verifiedCount: %v, requiredCount: %v", verifiedCount, requiredCount)
				return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusFail), ""
			}
		}

		iv.logger.V(4).Info("attestation checks passed", "path", path, "image", imageInfo.String(), "predicateType", attestation.PredicateType)
	}

	msg := fmt.Sprintf("verified image attestations for %s", image)
	iv.logger.V(2).Info(msg)
	return ruleResponse(*iv.rule, response.ImageVerify, msg, response.RuleStatusPass), imageInfo.Digest
}

func (iv *imageVerifier) verifyAttestorSet(
	ctx context.Context,
	attestorSet kyvernov1.AttestorSet,
	imageVerify kyvernov1.ImageVerification,
	imageInfo apiutils.ImageInfo,
	path string,
) (*cosign.Response, error) {
	var errorList []error
	verifiedCount := 0
	attestorSet = expandStaticKeys(attestorSet)
	requiredCount := getRequiredCount(attestorSet)
	image := imageInfo.String()

	for i, a := range attestorSet.Entries {
		var entryError error
		var cosignResp *cosign.Response
		attestorPath := fmt.Sprintf("%s.entries[%d]", path, i)
		iv.logger.V(4).Info("verifying attestorSet", "path", attestorPath)

		if a.Attestor != nil {
			nestedAttestorSet, err := kyvernov1.AttestorSetUnmarshal(a.Attestor)
			if err != nil {
				entryError = errors.Wrapf(err, "failed to unmarshal nested attestor %s", attestorPath)
			} else {
				attestorPath += ".attestor"
				cosignResp, entryError = iv.verifyAttestorSet(ctx, *nestedAttestorSet, imageVerify, imageInfo, attestorPath)
			}
		} else {
			opts, subPath := iv.buildOptionsAndPath(a, imageVerify, image, nil)
			cosignResp, entryError = cosign.VerifySignature(ctx, iv.rclient, *opts)
			if entryError != nil {
				entryError = errors.Wrapf(entryError, attestorPath+subPath)
			}
		}

		if entryError == nil {
			verifiedCount++
			if verifiedCount >= requiredCount {
				iv.logger.V(2).Info("image attestors verification succeeded", "verifiedCount", verifiedCount, "requiredCount", requiredCount)
				return cosignResp, nil
			}
		} else {
			errorList = append(errorList, entryError)
		}
	}

	err := multierr.Combine(errorList...)
	iv.logger.Info("image attestors verification failed", "verifiedCount", verifiedCount, "requiredCount", requiredCount, "errors", err.Error())
	return nil, err
}

func expandStaticKeys(attestorSet kyvernov1.AttestorSet) kyvernov1.AttestorSet {
	var entries []kyvernov1.Attestor
	for _, e := range attestorSet.Entries {
		if e.Keys != nil {
			keys := splitPEM(e.Keys.PublicKeys)
			if len(keys) > 1 {
				moreEntries := createStaticKeyAttestors(keys)
				entries = append(entries, moreEntries...)
				continue
			}
		}

		entries = append(entries, e)
	}

	return kyvernov1.AttestorSet{
		Count:   attestorSet.Count,
		Entries: entries,
	}
}

func splitPEM(pem string) []string {
	keys := strings.SplitAfter(pem, "-----END PUBLIC KEY-----")
	if len(keys) < 1 {
		return keys
	}

	return keys[0 : len(keys)-1]
}

func createStaticKeyAttestors(keys []string) []kyvernov1.Attestor {
	var attestors []kyvernov1.Attestor
	for _, k := range keys {
		a := kyvernov1.Attestor{
			Keys: &kyvernov1.StaticKeyAttestor{
				PublicKeys: k,
			},
		}
		attestors = append(attestors, a)
	}

	return attestors
}

func getRequiredCount(as kyvernov1.AttestorSet) int {
	if as.Count == nil || *as.Count == 0 {
		return len(as.Entries)
	}

	return *as.Count
}

func (iv *imageVerifier) buildOptionsAndPath(attestor kyvernov1.Attestor, imageVerify kyvernov1.ImageVerification, image string, attestation *kyvernov1.Attestation) (*cosign.Options, string) {
	path := ""
	opts := &cosign.Options{
		ImageRef:    image,
		Repository:  imageVerify.Repository,
		Annotations: imageVerify.Annotations,
	}

	if imageVerify.Roots != "" {
		opts.Roots = imageVerify.Roots
	}

	if attestation != nil {
		opts.PredicateType = attestation.PredicateType
		opts.FetchAttestations = true
	}

	if attestor.Keys != nil {
		path = path + ".keys"
		if attestor.Keys.PublicKeys != "" {
			opts.Key = attestor.Keys.PublicKeys
		} else if attestor.Keys.Secret != nil {
			opts.Key = fmt.Sprintf("k8s://%s/%s", attestor.Keys.Secret.Namespace,
				attestor.Keys.Secret.Name)
		} else if attestor.Keys.KMS != "" {
			opts.Key = attestor.Keys.KMS
		}
		if attestor.Keys.Rekor != nil {
			opts.RekorURL = attestor.Keys.Rekor.URL
		}
		opts.SignatureAlgorithm = attestor.Keys.SignatureAlgorithm
	} else if attestor.Certificates != nil {
		path = path + ".certificates"
		opts.Cert = attestor.Certificates.Certificate
		opts.CertChain = attestor.Certificates.CertificateChain
		if attestor.Certificates.Rekor != nil {
			opts.RekorURL = attestor.Certificates.Rekor.URL
		}
	} else if attestor.Keyless != nil {
		path = path + ".keyless"
		if attestor.Keyless.Rekor != nil {
			opts.RekorURL = attestor.Keyless.Rekor.URL
		}

		opts.Roots = attestor.Keyless.Roots
		opts.Issuer = attestor.Keyless.Issuer
		opts.Subject = attestor.Keyless.Subject
		opts.AdditionalExtensions = attestor.Keyless.AdditionalExtensions
	}

	if attestor.Repository != "" {
		opts.Repository = attestor.Repository
	}

	if attestor.Annotations != nil {
		opts.Annotations = attestor.Annotations
	}

	return opts, path
}

func makeAddDigestPatch(imageInfo apiutils.ImageInfo, digest string) ([]byte, error) {
	patch := make(map[string]interface{})
	patch["op"] = "replace"
	patch["path"] = imageInfo.Pointer
	patch["value"] = imageInfo.String() + "@" + digest
	return json.Marshal(patch)
}

func (iv *imageVerifier) verifyAttestation(statements []map[string]interface{}, attestation kyvernov1.Attestation, imageInfo apiutils.ImageInfo) error {
	if attestation.PredicateType == "" {
		return fmt.Errorf("a predicateType is required")
	}

	image := imageInfo.String()
	statementsByPredicate, types := buildStatementMap(statements)
	iv.logger.V(4).Info("checking attestations", "predicates", types, "image", image)

	statements = statementsByPredicate[attestation.PredicateType]
	if statements == nil {
		iv.logger.Info("no attestations found for predicate", "type", attestation.PredicateType, "predicates", types, "image", imageInfo.String())
		return fmt.Errorf("attestions not found for predicate type %s", attestation.PredicateType)
	}

	for _, s := range statements {
		iv.logger.Info("checking attestation", "predicates", types, "image", imageInfo.String())
		val, err := iv.checkAttestations(attestation, s)
		if err != nil {
			return errors.Wrap(err, "failed to check attestations")
		}

		if !val {
			return fmt.Errorf("attestation checks failed for %s and predicate %s", imageInfo.String(), attestation.PredicateType)
		}
	}

	return nil
}

func buildStatementMap(statements []map[string]interface{}) (map[string][]map[string]interface{}, []string) {
	results := map[string][]map[string]interface{}{}
	var predicateTypes []string
	for _, s := range statements {
		predicateType := s["predicateType"].(string)
		if results[predicateType] != nil {
			results[predicateType] = append(results[predicateType], s)
		} else {
			results[predicateType] = []map[string]interface{}{s}
		}

		predicateTypes = append(predicateTypes, predicateType)
	}

	return results, predicateTypes
}

func (iv *imageVerifier) checkAttestations(a kyvernov1.Attestation, s map[string]interface{}) (bool, error) {
	if len(a.Conditions) == 0 {
		return true, nil
	}

	iv.policyContext.jsonContext.Checkpoint()
	defer iv.policyContext.jsonContext.Restore()

	return evaluateConditions(a.Conditions, iv.policyContext.jsonContext, s, iv.logger)
}

func evaluateConditions(
	conditions []kyvernov1.AnyAllConditions,
	ctx enginecontext.Interface,
	s map[string]interface{},
	log logr.Logger,
) (bool, error) {
	predicate, ok := s["predicate"].(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("failed to extract predicate from statement: %v", s)
	}

	if err := enginecontext.AddJSONObject(ctx, predicate); err != nil {
		return false, errors.Wrapf(err, fmt.Sprintf("failed to add Statement to the context %v", s))
	}

	c, err := variables.SubstituteAllInConditions(log, ctx, conditions)
	if err != nil {
		return false, errors.Wrapf(err, "failed to substitute variables in attestation conditions")
	}

	pass := variables.EvaluateAnyAllConditions(log, ctx, c)
	return pass, nil
}
