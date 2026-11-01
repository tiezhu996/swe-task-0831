package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	client "example.com/swe-precheck-client"
)

func approvedPrecheckDesign(ctx context.Context, item client.ItemDetail) (client.PrecheckDesignRecord, error) {
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		return client.PrecheckDesignRecord{}, err
	}
	bound, err := registry.Bind(ctx, item)
	if err != nil {
		return client.PrecheckDesignRecord{}, err
	}
	if !bound {
		return client.PrecheckDesignRecord{}, os.ErrNotExist
	}
	return registry.Approved(ctx, item.ID, item.RepoURL, client.TextSHA256(item.RequirementPrompt))
}
func (w *workbenchServer) reusePrecheckDesign(ctx context.Context, path string) error {
	store := client.NewWorkflowStore(path)
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if state.Verification.DesignPath != "" {
		return nil
	}
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		return err
	}
	record, err := registry.Approved(ctx, state.Precheck.ItemID, state.Project.RepoURL, state.Precheck.PromptSHA256)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Materials.BaselineCommit != state.Project.BaselineCommit || record.Language != state.Project.Language {
		return fmt.Errorf("已记录设计的基线或语言与当前工作区不同，请核查，不能自动套用")
	}
	if state.Status != client.WorkflowPrecheckPassed {
		return fmt.Errorf("执行已开始，不能把配套设计重新标记为执行前冻结")
	}
	var schema client.FormSchema
	if client.ReadLocalJSON(state.Submission.SchemaPath, &schema) != nil {
		return fmt.Errorf("设计复用需实际字段规则")
	}
	_, err = store.FreezeDesign(ctx, record.Materials, schema)
	return err
}
func recordDraftForPrecheck(ctx context.Context, state client.WorkflowState, prompt, taskDir string, c *client.Client) error {
	registry, err := client.DefaultPrecheckDesignStore()
	if err != nil {
		return err
	}
	var materials client.DesignMaterials
	draft := filepath.Join(taskDir, "design-draft.json")
	if _, err := os.Stat(draft); err == nil {
		materials, err = client.ReadDesignMaterials(draft)
		if err != nil {
			return fmt.Errorf("出题设计草稿不可读")
		}
	} else {
		record, findErr := registry.Find(ctx, state.Project.RepoURL, prompt)
		if findErr != nil {
			return fmt.Errorf("新出题需同步四项设计；先保存 design-draft.json 或运行 precheck-design-save")
		}
		if record.Materials.BaselineCommit != state.Project.BaselineCommit || record.Language != state.Project.Language {
			return fmt.Errorf("预检草稿与已记录设计的基线或语言不一致")
		}
		return nil
	}
	if materials.RepoURL != state.Project.RepoURL || materials.BaselineCommit != state.Project.BaselineCommit {
		return fmt.Errorf("设计草稿不属于当前仓库与基线")
	}
	schema, err := c.SubmissionFormSchema(ctx)
	if err != nil {
		return fmt.Errorf("设计记录所需字段规则不可读")
	}
	_, err = registry.Save(ctx, prompt, state.Project.Language, materials, schema)
	return err
}
