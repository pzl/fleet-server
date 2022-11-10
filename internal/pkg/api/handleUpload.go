// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package api

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/fleet-server/v7/internal/pkg/bulk"
	"github.com/elastic/fleet-server/v7/internal/pkg/cache"
	"github.com/elastic/fleet-server/v7/internal/pkg/config"
	"github.com/elastic/fleet-server/v7/internal/pkg/limit"
	"github.com/elastic/fleet-server/v7/internal/pkg/logger"
	"github.com/elastic/fleet-server/v7/internal/pkg/upload"
	"github.com/elastic/fleet-server/v7/internal/pkg/upload/cbor"
	"github.com/elastic/go-elasticsearch/v7"
	"github.com/julienschmidt/httprouter"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// the only valid values of upload status according to storage spec
type UploadStatus string

const (
	UploadAwaiting UploadStatus = "AWAITING_UPLOAD"
	UploadProgress UploadStatus = "UPLOADING"
	UploadDone     UploadStatus = "READY"
	UploadFail     UploadStatus = "UPLOAD_ERROR"
	UploadDel      UploadStatus = "DELETED"
)

const (
	// TODO: move to a config
	maxParallelUploadOperations = 3
	maxParallelChunks           = 4
	maxFileSize                 = 104857600 // 100 MiB

)

func (rt Router) handleUploadStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	start := time.Now()

	reqID := r.Header.Get(logger.HeaderRequestID)

	zlog := log.With().
		Str(ECSHTTPRequestID, reqID).
		Logger()

	// authentication occurs inside here
	// to check that key agent ID matches the ID in the body payload yet-to-be unmarshalled
	err := rt.ut.handleUploadStart(&zlog, w, r)

	if err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)

		// Log this as warn for visibility that limit has been reached.
		// This allows customers to tune the configuration on detection of threshold.
		if errors.Is(err, limit.ErrMaxLimit) || errors.Is(err, upload.ErrMaxConcurrentUploads) {
			resp.Level = zerolog.WarnLevel
		}

		zlog.WithLevel(resp.Level).
			Err(err).
			Int(ECSHTTPResponseCode, resp.StatusCode).
			Int64(ECSEventDuration, time.Since(start).Nanoseconds()).
			Msg("fail upload initiation")

		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("fail writing error response")
		}
	}
}

func (rt Router) handleUploadChunk(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	start := time.Now()

	id := ps.ByName("id")
	chunkID := ps.ByName("num")

	reqID := r.Header.Get(logger.HeaderRequestID)

	zlog := log.With().
		Str(LogAgentID, id).
		Str(ECSHTTPRequestID, reqID).
		Logger()

	// simpler authentication check, since chunk checksum must
	// ultimately match the initial hash provided with the stricter key check
	if _, err := authAPIKey(r, rt.bulker, rt.ut.cache); err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)
		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("failed writing error response")
		}
		return
	}

	chunkNum, err := strconv.Atoi(chunkID)
	if err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)
		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("fail writing error response")
		}
		return
	}
	err = rt.ut.handleUploadChunk(&zlog, w, r, id, chunkNum)

	if err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)

		// Log this as warn for visibility that limit has been reached.
		// This allows customers to tune the configuration on detection of threshold.
		if errors.Is(err, limit.ErrMaxLimit) {
			resp.Level = zerolog.WarnLevel
		}

		zlog.WithLevel(resp.Level).
			Err(err).
			Int(ECSHTTPResponseCode, resp.StatusCode).
			Int64(ECSEventDuration, time.Since(start).Nanoseconds()).
			Msg("fail upload chunk")

		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("fail writing error response")
		}
	}
}

func (rt Router) handleUploadComplete(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	start := time.Now()

	id := ps.ByName("id")

	reqID := r.Header.Get(logger.HeaderRequestID)

	zlog := log.With().
		Str(LogAgentID, id).
		Str(ECSHTTPRequestID, reqID).
		Logger()

	// simpler authentication check, file integrity checksum
	// will catch directed tampering, this route just says "im done"
	if _, err := authAPIKey(r, rt.bulker, rt.ut.cache); err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)
		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("failed writing error response")
		}
		return
	}

	err := rt.ut.handleUploadComplete(&zlog, w, r, id)

	if err != nil {
		cntUpload.IncError(err)
		resp := NewHTTPErrResp(err)

		// Log this as warn for visibility that limit has been reached.
		// This allows customers to tune the configuration on detection of threshold.
		if errors.Is(err, limit.ErrMaxLimit) {
			resp.Level = zerolog.WarnLevel
		}

		zlog.WithLevel(resp.Level).
			Err(err).
			Int(ECSHTTPResponseCode, resp.StatusCode).
			Int64(ECSEventDuration, time.Since(start).Nanoseconds()).
			Msg("fail upload completion")

		if err := resp.Write(w); err != nil {
			zlog.Error().Err(err).Msg("fail writing error response")
		}
	}
}

type UploadT struct {
	bulker      bulk.Bulk
	chunkClient *elasticsearch.Client
	cache       cache.Cache
	upl         *upload.Uploader
}

func NewUploadT(cfg *config.Server, bulker bulk.Bulk, chunkClient *elasticsearch.Client, cache cache.Cache) *UploadT {
	log.Info().
		Interface("limits", cfg.Limits.ArtifactLimit).
		Int64("maxFileSize", maxFileSize).
		Int("maxParallelOps", maxParallelUploadOperations).
		Int("maxParallelChunks", maxParallelChunks).
		Msg("upload limits")

	return &UploadT{
		chunkClient: chunkClient,
		bulker:      bulker,
		cache:       cache,
		upl:         upload.New(maxFileSize, maxParallelChunks, maxParallelChunks),
	}
}

func (ut *UploadT) handleUploadStart(zlog *zerolog.Logger, w http.ResponseWriter, r *http.Request) error {

	// store raw body since we will json-decode twice
	// 2MB is a reasonable json payload size. Any more might be an indication of garbage
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	r.Body.Close()
	if err != nil {
		return fmt.Errorf("error reading request: %w", err)
	}

	// decode once here to access known fields we need to parse and work with
	var fi FileInfo
	if err := json.Unmarshal(body, &fi); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("file info body is required: %w", err)
		}
		return err
	}

	// check API key matches payload agent ID
	if _, err := authAgent(r, &fi.AgentID, ut.bulker, ut.cache); err != nil {
		return err
	}

	if err := validateUploadPayload(fi); err != nil {
		return err
	}

	docID := fmt.Sprintf("%s.%s", fi.ActionID, fi.AgentID)

	var hasher hash.Hash
	var sum string
	switch {
	case fi.File.Hash.SHA256 != "":
		hasher = sha256.New()
		sum = fi.File.Hash.SHA256
	case fi.File.Hash.MD5 != "":
		hasher = md5.New()
		sum = fi.File.Hash.MD5
	}

	op, err := ut.upl.Begin(fi.File.Size, docID, fi.Source, sum, hasher)
	if err != nil {
		return err
	}

	// second decode here to maintain the arbitrary shape and fields we will just pass through
	var reqDoc map[string]interface{}
	if err := json.Unmarshal(body, &reqDoc); err != nil {
		return fmt.Errorf("error parsing request json: %w", err)
	}

	doc, err := uploadRequestToFileDoc(reqDoc, op.ChunkSize)
	if err != nil {
		return fmt.Errorf("unable to convert request to file metadata doc: %w", err)
	}
	ret, err := upload.CreateFileDoc(r.Context(), ut.bulker, doc, fi.Source, docID)
	if err != nil {
		return err
	}

	zlog.Info().Str("return", ret).Msg("wrote doc")

	out, err := json.Marshal(map[string]interface{}{
		"upload_id":  op.ID,
		"chunk_size": op.ChunkSize,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(out)
	if err != nil {
		return err
	}
	return nil
}

func (ut *UploadT) handleUploadChunk(zlog *zerolog.Logger, w http.ResponseWriter, r *http.Request, uplID string, chunkID int) error {
	chunkInfo, err := ut.upl.Chunk(uplID, chunkID)
	if err != nil {
		return err
	}
	defer chunkInfo.Token.Release()
	if chunkInfo.FirstReceived {
		if err := updateUploadStatus(r.Context(), ut.bulker, chunkInfo.Upload, UploadProgress); err != nil {
			zlog.Warn().Err(err).Str("upload", uplID).Msg("unable to update upload status")
		}
	}

	// prevent over-sized chunks
	data := http.MaxBytesReader(w, r.Body, upload.MaxChunkSize)
	ce := cbor.NewChunkWriter(data, chunkInfo.Final, chunkInfo.Upload.DocID, chunkInfo.Upload.ChunkSize)
	if err := upload.IndexChunk(r.Context(), ut.chunkClient, ce, chunkInfo.Upload.Source, chunkInfo.Upload.DocID, chunkInfo.ID); err != nil {
		return err
	}
	return nil
}

func (ut *UploadT) handleUploadComplete(zlog *zerolog.Logger, w http.ResponseWriter, r *http.Request, uplID string) error {
	info, err := ut.upl.Complete(uplID, ut.bulker)
	if err != nil {
		return err
	}

	if err := updateUploadStatus(r.Context(), ut.bulker, info, UploadDone); err != nil {
		// should be 500 error probably?
		zlog.Warn().Err(err).Str("upload", uplID).Msg("unable to set upload status to complete")
		return err

	}

	_, err = w.Write([]byte(`{"status":"ok"}`))
	if err != nil {
		return err
	}
	return nil
}

// takes the arbitrary input document from an upload request and injects
// a few known fields as it passes through
func uploadRequestToFileDoc(req map[string]interface{}, chunkSize int64) ([]byte, error) {
	fileObj, ok := req["file"].(map[string]interface{})
	if !ok {
		return nil, errors.New("invalid upload request, file is not an object")
	}

	fileObj["ChunkSize"] = chunkSize
	fileObj["Status"] = string(UploadAwaiting)

	return json.Marshal(req)
}

func updateUploadStatus(ctx context.Context, bulker bulk.Bulk, info upload.Info, status UploadStatus) error {
	data, err := json.Marshal(map[string]interface{}{
		"doc": map[string]interface{}{
			"file": map[string]string{
				"Status": string(status),
			},
		},
	})
	if err != nil {
		return err
	}
	return upload.UpdateFileDoc(ctx, bulker, info.Source, info.DocID, data)
}

func validateUploadPayload(fi FileInfo) error {

	required := []struct {
		Field string
		Msg   string
	}{
		{fi.File.Name, "file name"},
		{fi.File.Mime, "mime_type"},
		{fi.ActionID, "action_id"},
		{fi.AgentID, "agent_id"},
		{fi.Source, "src"},
	}

	for _, req := range required {
		if strings.TrimSpace(req.Field) == "" {
			return fmt.Errorf("%s is required", req.Msg)
		}
	}

	//@todo: valid action?
	//@todo: valid src? will that make future expansion harder and require FS updates? maybe just validate the index exists

	if fi.File.Size <= 0 {
		return errors.New("invalid file size, size is required")
	}
	return nil
}
