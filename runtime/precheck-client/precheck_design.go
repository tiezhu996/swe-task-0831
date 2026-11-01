package precheckclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type PrecheckDesignRecord struct {
	SchemaVersion int             `json:"schema_version"`
	RecordID      string          `json:"record_id"`
	Prompt        string          `json:"prompt"`
	Language      string          `json:"language"`
	Materials     DesignMaterials `json:"materials"`
	RecordedAt    string          `json:"recorded_at"`
}
type precheckDesignBinding struct {
	SchemaVersion int    `json:"schema_version"`
	ItemID        int    `json:"item_id"`
	RecordID      string `json:"record_id"`
	RecordSHA256  string `json:"record_sha256"`
	BoundAt       string `json:"bound_at"`
}
type PrecheckDesignStore struct{ Root string }

func DefaultPrecheckDesignStore() (*PrecheckDesignStore, error) {
	root := os.Getenv("SWE_PRECHECK_DESIGN_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".local", "state", "swe-task", "precheck-designs")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("预检设计库必须是绝对路径")
	}
	return &PrecheckDesignStore{Root: root}, nil
}
func precheckDesignKey(repo, prompt string) string {
	return TextSHA256(repo + "\x00" + TextSHA256(prompt))
}
func (s *PrecheckDesignStore) locked(ctx context.Context, fn func() error) error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("预检设计库必须是私有普通目录")
	}
	file, err := os.OpenFile(filepath.Join(s.Root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}
func (s *PrecheckDesignStore) Save(ctx context.Context, prompt, language string, materials DesignMaterials, schema FormSchema) (PrecheckDesignRecord, error) {
	var result PrecheckDesignRecord
	err := s.locked(ctx, func() error {
		if CanonicalPrompt(prompt) != prompt || ValidateDraftPrompt(prompt) != nil || strings.TrimSpace(language) == "" {
			return fmt.Errorf("设计必须关联准确题面和实际语言")
		}
		if materials.Origin != "before_execution" || materials.FrozenAt != "" {
			return fmt.Errorf("出题设计必须是执行前草稿，不能冒充旧执行或冻结结果")
		}
		if !regexp.MustCompile(`^[0-9a-fA-F]{40}$`).MatchString(materials.BaselineCommit) {
			return fmt.Errorf("出题设计必须记录完整固定 commit")
		}
		if len(materials.SourceEvidence) > 32 {
			return fmt.Errorf("研究依据超过 32 个文件，请整理本题研究摘要")
		}
		for _, source := range materials.SourceEvidence {
			if source.Bytes <= 0 || source.Bytes > 16<<20 {
				return fmt.Errorf("研究摘要文件必须非空且不超过 16 MiB")
			}
		}
		state := WorkflowState{Status: WorkflowPrecheckPassed, Project: WorkflowProject{RepoURL: materials.RepoURL, BaselineCommit: materials.BaselineCommit}, Precheck: WorkflowPrecheck{PromptSHA256: TextSHA256(prompt)}}
		if err := validateDesign(materials, state, schema); err != nil {
			return err
		}
		key := precheckDesignKey(materials.RepoURL, prompt)
		target := filepath.Join(s.Root, "records", key)
		if _, err := os.Stat(target); err == nil {
			existing, err := s.readRecord(key)
			if err != nil {
				return err
			}
			// Record once; repeat invocation is harmless only for the same design.
			if existing.Language != language || existing.Materials.BaselineCommit != materials.BaselineCommit || !reflect.DeepEqual(existing.Materials.Fields, materials.Fields) || existing.Materials.Basis != materials.Basis {
				return fmt.Errorf("同题面已有不同设计，不能覆盖；修改题面时另建记录")
			}
			if err := verifyFileEvidence(existing.Materials.SourceEvidence); err != nil {
				return err
			}
			result = existing
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(filepath.Dir(target), ".design-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stage)
		stored := materials
		stored.Fields = map[string]string{}
		for k, v := range materials.Fields {
			stored.Fields[k] = v
		}
		stored.SourceEvidence = []WorkflowArtifact{}
		for i, source := range materials.SourceEvidence {
			data, err := os.ReadFile(source.Path)
			if err != nil || int64(len(data)) != source.Bytes || TextSHA256(string(data)) != source.SHA256 {
				return fmt.Errorf("研究文件在保存时变化，未记录设计")
			}
			name := fmt.Sprintf("basis-%03d%s", i, filepath.Ext(source.Path))
			if err := os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
				return err
			}
			stored.SourceEvidence = append(stored.SourceEvidence, WorkflowArtifact{Path: filepath.Join(target, name), SHA256: source.SHA256, Bytes: source.Bytes})
		}
		result = PrecheckDesignRecord{SchemaVersion: 1, RecordID: key, Prompt: prompt, Language: language, Materials: stored, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := SaveLocalJSON(filepath.Join(stage, "record.json"), result); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, "rubric.yaml"), []byte(stored.Fields["verify_rubric"]), 0600); err != nil {
			return err
		}
		return os.Rename(stage, target)
	})
	return result, err
}
func (s *PrecheckDesignStore) readRecord(key string, checkEvidence ...bool) (PrecheckDesignRecord, error) {
	var record PrecheckDesignRecord
	if !workflowHash.MatchString(key) {
		return record, fmt.Errorf("设计标识无效")
	}
	path := filepath.Join(s.Root, "records", key, "record.json")
	info, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return record, fmt.Errorf("设计记录不是私有普通文件")
	}
	if ReadLocalJSON(path, &record) != nil || record.SchemaVersion != 1 || record.RecordID != key || precheckDesignKey(record.Materials.RepoURL, record.Prompt) != key || record.Materials.PromptSHA256 != TextSHA256(record.Prompt) {
		return record, fmt.Errorf("设计记录原文或标识不一致")
	}
	if len(checkEvidence) == 0 || checkEvidence[0] {
		if err := verifyFileEvidence(record.Materials.SourceEvidence); err != nil {
			return record, err
		}
	}
	return record, nil
}
func (s *PrecheckDesignStore) Find(ctx context.Context, repo, prompt string) (PrecheckDesignRecord, error) {
	var record PrecheckDesignRecord
	err := s.locked(ctx, func() error { var err error; record, err = s.readRecord(precheckDesignKey(repo, prompt)); return err })
	return record, err
}
func (s *PrecheckDesignStore) Bind(ctx context.Context, item ItemDetail) (bool, error) {
	bound := false
	err := s.locked(ctx, func() error {
		if item.ID <= 0 || (item.Status != "PRECHECK_PASSED" && item.Status != "PRECHECK_CONSUMED") {
			return fmt.Errorf("只有平台实际通过的题面才能绑定设计")
		}
		key := precheckDesignKey(item.RepoURL, item.RequirementPrompt)
		record, err := s.readRecord(key)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if record.Materials.RepoURL != item.RepoURL || record.Prompt != item.RequirementPrompt {
			return fmt.Errorf("预检仓库或题面与设计不同")
		}
		artifact, err := HashWorkflowArtifact(filepath.Join(s.Root, "records", key, "record.json"))
		if err != nil {
			return err
		}
		path := filepath.Join(s.Root, "approved", fmt.Sprintf("%d.json", item.ID))
		var existing precheckDesignBinding
		if err := ReadLocalJSON(path, &existing); err == nil {
			if existing.ItemID != item.ID || existing.RecordID != key || existing.RecordSHA256 != artifact.SHA256 {
				return fmt.Errorf("已有预检设计绑定冲突")
			}
			bound = true
			return nil
		}
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("已有设计绑定不可读，不能覆盖")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := SaveLocalJSON(path, precheckDesignBinding{SchemaVersion: 1, ItemID: item.ID, RecordID: key, RecordSHA256: artifact.SHA256, BoundAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
			return err
		}
		bound = true
		return nil
	})
	return bound, err
}
func (s *PrecheckDesignStore) Approved(ctx context.Context, id int, repo, promptHash string, checkEvidence ...bool) (PrecheckDesignRecord, error) {
	var record PrecheckDesignRecord
	err := s.locked(ctx, func() error {
		var binding precheckDesignBinding
		path := filepath.Join(s.Root, "approved", fmt.Sprintf("%d.json", id))
		if _, err := os.Lstat(path); err != nil {
			return err
		}
		if ReadLocalJSON(path, &binding) != nil || binding.SchemaVersion != 1 || binding.ItemID != id {
			return fmt.Errorf("通过设计绑定无效")
		}
		var err error
		record, err = s.readRecord(binding.RecordID, checkEvidence...)
		if err != nil {
			return err
		}
		hash, err := HashWorkflowArtifact(filepath.Join(s.Root, "records", binding.RecordID, "record.json"))
		if err != nil || hash.SHA256 != binding.RecordSHA256 || record.Materials.RepoURL != repo || record.Materials.PromptSHA256 != promptHash {
			return fmt.Errorf("设计记录与当前预检绑定不一致")
		}
		return nil
	})
	return record, err
}
func (s *PrecheckDesignStore) RecordPath(record PrecheckDesignRecord) string {
	return filepath.Join(s.Root, "records", record.RecordID, "record.json")
}
func ReadDesignMaterials(path string) (DesignMaterials, error) {
	var m DesignMaterials
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

func (s *PrecheckDesignStore) Repositories(ctx context.Context) (map[string]bool, error) {
	repos := map[string]bool{}
	err := s.locked(ctx, func() error {
		entries, err := os.ReadDir(filepath.Join(s.Root, "records"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !workflowHash.MatchString(entry.Name()) {
				continue
			}
			var record PrecheckDesignRecord
			if ReadLocalJSON(filepath.Join(s.Root, "records", entry.Name(), "record.json"), &record) == nil {
				repos[record.Materials.RepoURL] = true
			}
		}
		return nil
	})
	return repos, err
}
