package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/primandproper/platform-go/v9/observability/logging"
)

// wireEncoder is beeline's encoding.ServerEncoderDecoder: plain JSON with this
// API's established bytes. It deliberately diverges from the platform default
// in two ways — decoding is lenient (no DisallowUnknownFields) and bounded by
// maxGeoJSONBytes, and encoding matches writeJSON exactly — so swapping the
// router library changes no request or response byte.
type wireEncoder struct {
	logger logging.Logger
}

func newWireEncoder(logger logging.Logger) *wireEncoder {
	return &wireEncoder{logger: logging.EnsureLogger(logger)}
}

func (e *wireEncoder) RespondWithData(ctx context.Context, res http.ResponseWriter, val any) {
	e.EncodeResponseWithStatus(ctx, res, val, http.StatusOK)
}

func (e *wireEncoder) EncodeResponseWithStatus(_ context.Context, res http.ResponseWriter, val any, statusCode int) {
	writeJSON(res, e.logger, statusCode, val)
}

func (e *wireEncoder) DecodeRequest(_ context.Context, req *http.Request, dest any) error {
	return decodeJSON(req, dest)
}

func (e *wireEncoder) DecodeBytes(_ context.Context, payload []byte, dest any) error {
	return json.Unmarshal(payload, dest)
}

func (e *wireEncoder) MustEncode(_ context.Context, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return b
}

func (e *wireEncoder) MustEncodeJSON(ctx context.Context, v any) []byte {
	return e.MustEncode(ctx, v)
}
