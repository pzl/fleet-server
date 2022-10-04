// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/fleet-server/v7/internal/pkg/bulk"
	"github.com/elastic/fleet-server/v7/internal/pkg/cache"
	"github.com/elastic/fleet-server/v7/internal/pkg/config"
	"github.com/elastic/fleet-server/v7/internal/pkg/dl"
	"github.com/elastic/fleet-server/v7/internal/pkg/limit"
	"github.com/elastic/fleet-server/v7/internal/pkg/logger"
	"github.com/elastic/fleet-server/v7/internal/pkg/model"
	"github.com/elastic/fleet-server/v7/internal/pkg/upload"
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
)

func (rt Router) handleUploadStart(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	start := time.Now()

	reqID := r.Header.Get(logger.HeaderRequestID)

	zlog := log.With().
		Str(ECSHTTPRequestID, reqID).
		Logger()

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
		Int("maxParallelOps", maxParallelUploadOperations).
		Int("maxParallelChunks", maxParallelChunks).
		Msg("Artifact install limits")

	return &UploadT{
		chunkClient: chunkClient,
		bulker:      bulker,
		cache:       cache,
		upl:         upload.New(maxParallelChunks, maxParallelChunks),
	}
}

func (ut *UploadT) handleUploadStart(zlog *zerolog.Logger, w http.ResponseWriter, r *http.Request) error {
	var fi FileInfo
	if err := json.NewDecoder(r.Body).Decode(&fi); err != nil {
		r.Body.Close()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("file info body is required: %w", err)
		}
		return err
	}
	r.Body.Close()

	if strings.TrimSpace(fi.File.Name) == "" {
		return errors.New("file name is required")
	}
	if fi.File.Size <= 0 {
		return errors.New("invalid file size, size is required")
	}
	if strings.TrimSpace(fi.File.Mime) == "" {
		return errors.New("mime_type is required")
	}

	op, err := ut.upl.Begin(fi.File.Size)
	if err != nil {
		return err
	}

	doc := uploadRequestToFileInfo(fi, op.ChunkSize)
	ret, err := dl.CreateUploadInfo(r.Context(), ut.bulker, doc, op.ID) // @todo: replace uploadID with correct file base ID
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
		if err := updateUploadStatus(r.Context(), ut.bulker, uplID, UploadProgress); err != nil {
			zlog.Warn().Err(err).Str("upload", uplID).Msg("unable to update upload status")
		}
	}

	// prevent over-sized chunks
	data := http.MaxBytesReader(w, r.Body, upload.MaxChunkSize)
	if err := dl.UploadChunk(r.Context(), ut.chunkClient, data, chunkInfo); err != nil {
		return err
	}
	return nil
}

func (ut *UploadT) handleUploadComplete(zlog *zerolog.Logger, w http.ResponseWriter, r *http.Request, uplID string) error {
	data, err := ut.upl.Complete(uplID)
	if err != nil {
		return err
	}

	if err := updateUploadStatus(r.Context(), ut.bulker, uplID, UploadDone); err != nil {
		// should be 500 error probably?
		zlog.Warn().Err(err).Str("upload", uplID).Msg("unable to set upload status to complete")
		return err

	}

	_, err = w.Write([]byte(data))
	if err != nil {
		return err
	}
	return nil
}

func uploadRequestToFileInfo(req FileInfo, chunkSize int64) model.FileInfo {
	return model.FileInfo{
		File: &model.FileMetadata{
			Accessed:    req.File.Accessed,
			Attributes:  req.File.Attributes,
			ChunkSize:   chunkSize,
			Compression: req.File.Compression,
			Created:     req.File.Created,
			Ctime:       req.File.CTime,
			Device:      req.File.Device,
			Directory:   req.File.Directory,
			DriveLetter: req.File.DriveLetter,
			Extension:   req.File.Extension,
			Gid:         req.File.GID,
			Group:       req.File.Group,
			Hash: &model.Hash{
				Sha256: req.File.Hash.SHA256,
			},
			Inode:      req.File.INode,
			MimeType:   req.File.Mime,
			Mode:       req.File.Mode,
			Mtime:      req.File.MTime,
			Name:       req.File.Name,
			Owner:      req.File.Owner,
			Path:       req.File.Path,
			Size:       req.File.Size,
			Status:     string(UploadAwaiting),
			TargetPath: req.File.TargetPath,
			Type:       req.File.Type,
			Uid:        req.File.UID,
		},
	}
}

func updateUploadStatus(ctx context.Context, bulker bulk.Bulk, fileID string, status UploadStatus) error {
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
	return dl.UpdateUpload(ctx, bulker, fileID, data)
}
