package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"time"

	url2 "net/url"

	m "github.com/minio/minio-go/v7"

	"github.com/deepfence/ThreatMapper/deepfence_server/controls"
	"github.com/deepfence/ThreatMapper/deepfence_server/model"
	"github.com/deepfence/ThreatMapper/deepfence_utils/directory"
	"github.com/deepfence/ThreatMapper/deepfence_utils/log"
	"github.com/deepfence/ThreatMapper/deepfence_utils/utils"
	httpext "github.com/go-playground/pkg/v5/net/http"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
	"golang.org/x/mod/semver"
)

func (h *Handler) UploadAgentBinaries(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	ctx := r.Context()

	if err := r.ParseMultipartForm(500 * 1024 * 1024); err != nil {
		h.respondError(&BadDecoding{err}, w)
		return
	}
	file, fileHeader, err := r.FormFile("tarball")
	if err != nil {
		h.respondError(&BadDecoding{err}, w)
		return
	}
	defer file.Close()

	filename := filepath.Base(fileHeader.Filename)
	vername := filename[:len(filename)-len(filepath.Ext(filename))]
	if !semver.IsValid(vername) {
		h.respondError(&BadDecoding{fmt.Errorf("tarball name should be versioned %v", vername)}, w)
	}

	log.Info().Msgf("uploaded file content type %s", fileHeader.Header.Get("Content-Type"))
	if (fileHeader.Header.Get("Content-Type")) != "application/gzip" {
		h.respondError(&contentTypeError, w)
		return
	}

	tarball, err := io.ReadAll(file)
	if err != nil {
		h.respondError(&BadDecoding{err}, w)
		return
	}
	versioned_tarball := map[string]*bytes.Buffer{
		vername: bytes.NewBuffer(tarball),
	}

	tags_with_urls, err := PrepareAgentBinariesReleases(ctx, versioned_tarball)
	if err != nil {
		h.respondError(&InternalServerError{err}, w)
		return
	}

	err = IngestAgentVersion(ctx, tags_with_urls)
	if err != nil {
		h.respondError(&InternalServerError{err}, w)
		return
	}

	err = ScheduleAutoUpgradeForPatchChanges(ctx, GetLatestVersionByMajorMinor(versioned_tarball))
	if err != nil {
		h.respondError(&InternalServerError{err}, w)
		return
	}

	_ = httpext.JSON(w, http.StatusOK, nil)
}

func PrepareAgentBinariesReleases(ctx context.Context, versioned_tarball map[string]*bytes.Buffer) (map[string]string, error) {
	processed_tags := map[string]string{}
	minio, err := directory.MinioClient(ctx)
	if err != nil {
		return processed_tags, err
	}

	for version, b := range versioned_tarball {
		res, err := minio.UploadFile(ctx,
			version,
			b.Bytes(),
			false,
			m.PutObjectOptions{ContentType: "application/gzip"})
		key := ""
		if err != nil {
			ape, ok := err.(directory.AlreadyPresentError)
			if ok {
				log.Warn().Err(err).Msg("Skip upload")
				key = ape.Path
			} else {
				log.Error().Err(err).Msg("Upload")
				continue
			}
		} else {
			key = res.Key
		}

		url, err := minio.ExposeFile(ctx, key, false, 10*time.Hour, url2.Values{})
		if err != nil {
			log.Error().Err(err)
			continue
		}
		log.Debug().Msgf("Exposed URL: %v", url)
		processed_tags[version] = url
	}
	return processed_tags, nil
}

func IngestAgentVersion(ctx context.Context, tags_to_url map[string]string) error {
	nc, err := directory.Neo4jClient(ctx)
	if err != nil {
		return err
	}
	session := nc.NewSession(neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close()

	tx, err := session.BeginTransaction(neo4j.WithTxTimeout(15 * time.Second))
	if err != nil {
		return err
	}
	defer tx.Close()

	tags_to_ingest := []map[string]string{}
	for k, v := range tags_to_url {
		tags_to_ingest = append(tags_to_ingest, map[string]string{"tag": k, "url": v})
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MERGE (n:AgentVersion{node_id: row.tag})
		SET n.url = row.url`,
		map[string]interface{}{"batch": tags_to_ingest}); err != nil {
		return err
	}

	return tx.Commit()
}

func ScheduleAutoUpgradeForPatchChanges(ctx context.Context, latest map[string]string) error {
	nc, err := directory.Neo4jClient(ctx)
	if err != nil {
		return err
	}
	session := nc.NewSession(neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close()

	tx, err := session.BeginTransaction(neo4j.WithTxTimeout(15 * time.Second))
	if err != nil {
		return err
	}
	defer tx.Close()

	tags_to_ingest := []map[string]string{}
	for k, v := range latest {
		action, err := controls.PrepareAgentUpgradeAction(ctx, v)
		if err != nil {
			log.Error().Msg(err.Error())
			continue
		}

		action_str, err := json.Marshal(action)
		if err != nil {
			return err
		}
		tags_to_ingest = append(tags_to_ingest,
			map[string]string{
				"major_minor": k,
				"latest":      v,
				"action":      string(action_str)})
	}

	if _, err = tx.Run(`
		UNWIND $batch as row
		MATCH (vnew:AgentVersion{node_id: row.latest})
		MATCH (v:AgentVersion) <-[:VERSIONED]- (n:Node)
		WHERE v.node_id STARTS WITH row.major_minor
		AND v.node_id <> row.latest
		MERGE (vnew) -[:SCHEDULED{status: $status, retries: 0, trigger_action: row.action, updated_at: TIMESTAMP()}]-> (n)`,
		map[string]interface{}{
			"status": utils.SCAN_STATUS_STARTING,
			"batch":  tags_to_ingest}); err != nil {
		return err
	}

	return tx.Commit()
}

func GetLatestVersionByMajorMinor(versions map[string]*bytes.Buffer) map[string]string {
	latest_patches := map[string]string{}
	all_versions := map[string][]string{}

	for k := range versions {
		all_versions[semver.MajorMinor(k)] =
			append(all_versions[semver.MajorMinor(k)],
				k)
	}
	for k, v := range all_versions {
		semver.Sort(v)
		latest_patches[k] = v[len(v)-1]
	}
	return latest_patches
}

func (h *Handler) ListAgentVersion(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	list, err := GetAgentVersionList(r.Context())
	if err != nil {
		h.respondError(&InternalServerError{err}, w)
		return
	}

	_ = httpext.JSON(w, http.StatusOK, model.ListAgentVersionResp{Versions: list})
}

func GetAgentVersionList(ctx context.Context) ([]string, error) {
	nc, err := directory.Neo4jClient(ctx)
	if err != nil {
		return nil, err
	}
	session := nc.NewSession(neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close()

	tx, err := session.BeginTransaction(neo4j.WithTxTimeout(15 * time.Second))
	if err != nil {
		return nil, err
	}
	defer tx.Close()

	res, err := tx.Run(`
		MATCH (n:AgentVersion) 
		RETURN n.node_id`,
		map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	recs, err := res.Collect()
	if err != nil {
		return nil, err
	}

	versions := []string{}
	for _, rec := range recs {
		versions = append(versions, rec.Values[0].(string))
	}

	return versions, nil
}