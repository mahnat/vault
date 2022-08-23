package pki

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/hashicorp/vault/sdk/helper/errutil"

	"golang.org/x/crypto/ocsp"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	ocspReqParam            = "req"
	ocspResponseContentType = "application/ocsp-response"
)

type ocspRespInfo struct {
	formattedSerialNumber string
	serialNumber          *big.Int
	ocspStatus            int
	revocationTimeUTC     *time.Time
	issuerID              issuerID
}

// These response variables should not be mutated, instead treat them as constants
var (
	OcspUnauthorizedResponse = &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: ocspResponseContentType,
			logical.HTTPStatusCode:  http.StatusUnauthorized,
			logical.HTTPRawBody:     ocsp.UnauthorizedErrorResponse,
		},
	}
	OcspMalformedResponse = &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: ocspResponseContentType,
			logical.HTTPStatusCode:  http.StatusBadRequest,
			logical.HTTPRawBody:     ocsp.MalformedRequestErrorResponse,
		},
	}
	OcspInternalErrorResponse = &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: ocspResponseContentType,
			logical.HTTPStatusCode:  http.StatusInternalServerError,
			logical.HTTPRawBody:     ocsp.InternalErrorErrorResponse,
		},
	}

	ErrMissingOcspUsage = errors.New("issuer entry did not have the OCSPSigning usage")
	ErrIssuerHasNoKey   = errors.New("issuer has no key")
	ErrUnknownIssuer    = errors.New("unknown issuer")
)

func buildPathOcspGet(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "ocsp/" + framework.MatchAllRegex(ocspReqParam),
		Fields: map[string]*framework.FieldSchema{
			ocspReqParam: {
				Type:        framework.TypeString,
				Description: "base-64 encoded ocsp request",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.ocspHandler,
			},
		},

		HelpSynopsis:    pathOcspHelpSyn,
		HelpDescription: pathOcspHelpDesc,
	}
}

func buildPathOcspPost(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "ocsp",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.ocspHandler,
			},
		},

		HelpSynopsis:    pathOcspHelpSyn,
		HelpDescription: pathOcspHelpDesc,
	}
}

func (b *backend) ocspHandler(ctx context.Context, request *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	sc := b.makeStorageContext(ctx, request.Storage)
	cfg, err := b.crlBuilder.getConfigWithUpdate(sc)
	if err != nil || cfg.OcspDisable {
		return OcspUnauthorizedResponse, nil
	}

	derReq, err := fetchDerEncodedRequest(request, data)
	if err != nil {
		return OcspMalformedResponse, nil
	}

	ocspReq, err := ocsp.ParseRequest(derReq)
	if err != nil {
		return OcspMalformedResponse, nil
	}

	ocspStatus, err := getOcspStatus(sc, request, ocspReq)
	if err != nil {
		return logAndReturnInternalError(b, err), nil
	}

	caBundle, err := lookupOcspIssuer(sc, ocspReq, ocspStatus.issuerID)
	if err != nil {
		if errors.Is(err, ErrUnknownIssuer) {
			// Since we were not able to find a matching issuer for the incoming request
			// generate an Unknown OCSP response. This might turn into an Unauthorized if
			// we find out that we don't have a default issuer or it's missing the proper Usage flags
			return generateUnknownResponse(sc, ocspReq), nil
		}
		if errors.Is(err, ErrMissingOcspUsage) {
			// If we did find a matching issuer but aren't allowed to sign, the spec says
			// we should be responding with an Unauthorized response as we don't have the
			// ability to sign the response.
			// https://www.rfc-editor.org/rfc/rfc5019#section-2.2.3
			return OcspUnauthorizedResponse, nil
		}
		return logAndReturnInternalError(b, err), nil
	}

	byteResp, err := genResponse(caBundle, ocspStatus, ocspReq.HashAlgorithm)
	if err != nil {
		return logAndReturnInternalError(b, err), nil
	}

	return &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: ocspResponseContentType,
			logical.HTTPStatusCode:  http.StatusOK,
			logical.HTTPRawBody:     byteResp,
		},
	}, nil
}

func generateUnknownResponse(sc *storageContext, ocspReq *ocsp.Request) *logical.Response {
	// Generate an Unknown OCSP response, signing with the default issuer from the mount as we did
	// not match the request's issuer. If no default issuer can be used, return with Unauthorized as there
	// isn't much else we can do at this point.
	config, err := sc.getIssuersConfig()
	if err != nil {
		return logAndReturnInternalError(sc.Backend, err)
	}

	if config.DefaultIssuerId == "" {
		// If we don't have any issuers or default issuers set, no way to sign a response so Unauthorized it is.
		return OcspUnauthorizedResponse
	}

	caBundle, issuer, err := getOcspIssuerParsedBundle(sc, config.DefaultIssuerId)
	if err != nil {
		if errors.Is(err, ErrUnknownIssuer) || errors.Is(err, ErrIssuerHasNoKey) {
			// We must have raced on a delete/update of the default issuer, anyways
			// no way to sign a response so Unauthorized it is.
			return OcspUnauthorizedResponse
		}
		return logAndReturnInternalError(sc.Backend, err)
	}

	if !issuer.Usage.HasUsage(OCSPSigningUsage) {
		// If we don't have any issuers or default issuers set, no way to sign a response so Unauthorized it is.
		return OcspUnauthorizedResponse
	}

	info := &ocspRespInfo{
		serialNumber: ocspReq.SerialNumber,
		ocspStatus:   ocsp.Unknown,
	}

	byteResp, err := genResponse(caBundle, info, ocspReq.HashAlgorithm)
	if err != nil {
		return logAndReturnInternalError(sc.Backend, err)
	}

	return &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: ocspResponseContentType,
			logical.HTTPStatusCode:  http.StatusOK,
			logical.HTTPRawBody:     byteResp,
		},
	}
}

func fetchDerEncodedRequest(request *logical.Request, data *framework.FieldData) ([]byte, error) {
	switch request.Operation {
	case logical.ReadOperation:
		// The param within the GET request should have a base64 encoded version of a DER request.
		base64Req := data.Get(ocspReqParam).(string)
		if base64Req == "" {
			return nil, errors.New("no base64 encoded ocsp request was found")
		}

		return base64.StdEncoding.DecodeString(base64Req)
	case logical.UpdateOperation:
		// POST bodies should contain the binary form of the DER request.
		rawBody := request.HTTPRequest.Body
		defer rawBody.Close()

		buf := bytes.Buffer{}
		_, err := buf.ReadFrom(rawBody)
		if err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("unsupported request method: %s", request.HTTPRequest.Method)
	}
}

func logAndReturnInternalError(b *backend, err error) *logical.Response {
	// Since OCSP might be a high traffic endpoint, we will log at debug level only
	// any internal errors we do get. There is no way for us to return to the end-user
	// errors, so we rely on the log statement to help in debugging possible
	// issues in the field.
	b.Logger().Debug("OCSP internal error", "error", err)
	return OcspInternalErrorResponse
}

func getOcspStatus(sc *storageContext, request *logical.Request, ocspReq *ocsp.Request) (*ocspRespInfo, error) {
	revEntryRaw, err := fetchCertBySerialBigInt(sc.Context, sc.Backend, request, revokedPath, ocspReq.SerialNumber)
	if err != nil {
		return nil, err
	}

	info := ocspRespInfo{
		serialNumber: ocspReq.SerialNumber,
		ocspStatus:   ocsp.Good,
	}

	if revEntryRaw != nil {
		var revEntry revocationInfo
		if err := revEntryRaw.DecodeJSON(&revEntry); err != nil {
			return nil, err
		}

		info.ocspStatus = ocsp.Revoked
		info.revocationTimeUTC = &revEntry.RevocationTimeUTC
		info.issuerID = revEntry.CertificateIssuer // This might be empty if the CRL hasn't been rebuilt
	}

	return &info, nil
}

func lookupOcspIssuer(sc *storageContext, req *ocsp.Request, optRevokedIssuer issuerID) (*certutil.ParsedCertBundle, error) {
	reqHash := req.HashAlgorithm
	if !reqHash.Available() {
		return nil, x509.ErrUnsupportedAlgorithm
	}

	// This will prime up issuerIds, with either the optRevokedIssuer value if set
	// or if we are operating in legacy storage mode, the shim bundle id or finally
	// a list of all our issuers in this mount.
	issuerIds, err := lookupIssuerIds(sc, optRevokedIssuer)
	if err != nil {
		return nil, err
	}

	for _, issuerId := range issuerIds {
		parsedBundle, issuer, err := getOcspIssuerParsedBundle(sc, issuerId)
		if err != nil {
			// A bit touchy here as if we get an ErrUnknownIssuer for an issuer id that we picked up
			// from a revocation entry, we still return an ErrUnknownOcspIssuer as we can't validate
			// the end-user actually meant this specific issuer's cert with serial X.
			if errors.Is(err, ErrUnknownIssuer) || errors.Is(err, ErrIssuerHasNoKey) {
				// This skips either bad issuer ids, or root certs with no keys that we can't use.
				continue
			}
			return nil, err
		}

		// Make sure the client and Vault are talking about the same issuer, otherwise
		// we might have a case of a matching serial number for a different issuer which
		// we should not respond back in the affirmative about.
		matches, err := doesRequestMatchIssuer(parsedBundle, req)
		if err != nil {
			return nil, err
		}

		if matches {
			if !issuer.Usage.HasUsage(OCSPSigningUsage) {
				// We found the correct issuer, but it's not allowed to sign the
				// response so give up.
				return nil, ErrMissingOcspUsage
			}

			return parsedBundle, nil
		}
	}

	return nil, ErrUnknownIssuer
}

func getOcspIssuerParsedBundle(sc *storageContext, issuerId issuerID) (*certutil.ParsedCertBundle, *issuerEntry, error) {
	issuer, bundle, err := sc.fetchCertBundleByIssuerId(issuerId, true)
	if err != nil {
		switch err.(type) {
		case errutil.UserError:
			// Most likely the issuer id no longer exists skip it
			return nil, nil, ErrUnknownIssuer
		default:
			return nil, nil, err
		}
	}

	if issuer.KeyID == "" {
		// No point if the key does not exist from the issuer to use as a signer.
		return nil, nil, ErrIssuerHasNoKey
	}

	caBundle, err := parseCABundle(sc.Context, sc.Backend, bundle)
	if err != nil {
		return nil, nil, err
	}

	return caBundle, issuer, nil
}

func lookupIssuerIds(sc *storageContext, optRevokedIssuer issuerID) ([]issuerID, error) {
	if optRevokedIssuer != "" {
		return []issuerID{optRevokedIssuer}, nil
	}

	if sc.Backend.useLegacyBundleCaStorage() {
		return []issuerID{legacyBundleShimID}, nil
	}

	return sc.listIssuers()
}

func doesRequestMatchIssuer(parsedBundle *certutil.ParsedCertBundle, req *ocsp.Request) (bool, error) {
	var pkInfo struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(parsedBundle.Certificate.RawSubjectPublicKeyInfo, &pkInfo); err != nil {
		return false, err
	}

	h := req.HashAlgorithm.New()
	h.Write(pkInfo.PublicKey.RightAlign())
	issuerKeyHash := h.Sum(nil)

	h.Reset()
	h.Write(parsedBundle.Certificate.RawSubject)
	issuerNameHash := h.Sum(nil)

	return bytes.Equal(req.IssuerKeyHash, issuerKeyHash) && bytes.Equal(req.IssuerNameHash, issuerNameHash), nil
}

func genResponse(caBundle *certutil.ParsedCertBundle, info *ocspRespInfo, reqHash crypto.Hash) ([]byte, error) {
	curTime := time.Now()
	template := ocsp.Response{
		IssuerHash:      reqHash,
		Status:          info.ocspStatus,
		SerialNumber:    info.serialNumber,
		ThisUpdate:      curTime,
		NextUpdate:      curTime,
		Certificate:     caBundle.Certificate,
		ExtraExtensions: []pkix.Extension{},
	}

	if info.ocspStatus == ocsp.Revoked {
		template.RevokedAt = *info.revocationTimeUTC
		template.RevocationReason = ocsp.Unspecified
	}

	return ocsp.CreateResponse(caBundle.Certificate, caBundle.Certificate, template, caBundle.PrivateKey)
}

const pathOcspHelpSyn = `
Query a certificate's revocation status through OCSP'
`

const pathOcspHelpDesc = `
This endpoint expects DER encoded OCSP requests and returns DER encoded OCSP responses
`
