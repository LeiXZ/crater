//nolint:gocritic,gocyclo,lll,mnd,revive // Patrol orchestration intentionally centralizes policy execution and thresholds.
package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/raids-lab/crater/dao/model"
	"github.com/raids-lab/crater/dao/query"
	"github.com/raids-lab/crater/pkg/ceph"
	"github.com/raids-lab/crater/pkg/config"
	"github.com/raids-lab/crater/pkg/monitor"
	"github.com/raids-lab/crater/pkg/util"
)

const (
	// 占卡检测任务
	TRIGGER_GPU_ANALYSIS_JOB = "trigger-gpu-analysis-job"
	// Billing 基础循环
	TRIGGER_BILLING_BASE_LOOP_JOB = "biling-base-loop"
	// 存储告警 AI 分析任务
	ANALYZE_STORAGE_ALERTS         = "analyze-storage-alerts"
	AUTO_SHRINK_STORAGE_EXPANSIONS = "auto-shrink-storage-expansions"

	// AI 分析最大并发数
	defaultMaxConcurrentStorageAnalysis = 3
	autoShrinkToBufferThreshold         = 0.90
	autoShrinkRecoverThreshold          = 0.80
	autoShrinkObservationWindow         = time.Hour
	autoShrinkStageExpanded             = "expanded"
	autoShrinkStageBuffer               = "buffer_reduction"

	storageAnalysisConcurrencyEnv = "CRATER_STORAGE_ANALYSIS_MAX_CONCURRENCY"
)

type GpuAnalysisServiceInterface interface {
	TriggerAllJobsAnalysis(ctx context.Context) (int, error)
}

type BillingServiceInterface interface {
	RunBaseLoopOnce(ctx context.Context) (any, error)
}

// AgentDecision 是 LLM 存储扩容决策的结果，定义在 patrol 包以避免循环依赖。
type AgentDecision struct {
	AllowExpand   bool
	ExpandBytes   int64
	FreezeNewJobs bool
	Reason        string
	DecisionJobID string
}

// StorageAgentFunc 是调用 LLM 进行存储分析的函数签名。
type StorageAgentFunc func(tenantID string) (*AgentDecision, error)

type StorageAgentStartFunc func(ctx context.Context, tenantID string) (string, error)

type StorageAgentAwaitFunc func(ctx context.Context, tenantID string, jobID string) (*AgentDecision, error)

// Clients 包含巡检任务所需的客户端
type Clients struct {
	Client             client.Client
	KubeClient         kubernetes.Interface
	KubeConfig         *rest.Config
	PromClient         monitor.PrometheusInterface
	GpuAnalysisService GpuAnalysisServiceInterface
	BillingService     BillingServiceInterface
	RecordDecision     func(ctx context.Context, jobID string, action string, runErr error)
	StorageAgent       StorageAgentFunc // 注入的 LLM 分析函数，nil 时跳过 AI 分析
	StorageAgentStart  StorageAgentStartFunc
	StorageAgentAwait  StorageAgentAwaitFunc
}

func storageAnalysisConcurrency() int {
	raw := os.Getenv(storageAnalysisConcurrencyEnv)
	if raw == "" {
		return defaultMaxConcurrentStorageAnalysis
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		klog.Warningf(
			"storageAnalysisConcurrency: invalid %s=%q, fallback to %d",
			storageAnalysisConcurrencyEnv,
			raw,
			defaultMaxConcurrentStorageAnalysis,
		)
		return defaultMaxConcurrentStorageAnalysis
	}

	return value
}

func NewPatrolClients(
	cli client.Client,
	kubeClient kubernetes.Interface,
	kubeConfig *rest.Config,
	promClient monitor.PrometheusInterface,
	gpuAnalysisService GpuAnalysisServiceInterface,
	billingService BillingServiceInterface,
) *Clients {
	return &Clients{
		Client:             cli,
		KubeClient:         kubeClient,
		KubeConfig:         kubeConfig,
		PromClient:         promClient,
		GpuAnalysisService: gpuAnalysisService,
		BillingService:     billingService,
	}
}

type StorageUsageRefreshResult struct {
	Updated     int       `json:"updated"`
	Failed      int       `json:"failed"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

// RefreshUserSpaceSizes refreshes the cached CephFS usage after an explicit admin request.
func RefreshUserSpaceSizes(ctx context.Context, clients *Clients) (StorageUsageRefreshResult, error) {
	refreshResult := StorageUsageRefreshResult{}
	if !ceph.StorageQuotaEnabled() {
		return refreshResult, fmt.Errorf("storage quota usage refresh is disabled")
	}

	var users []model.User
	db := query.GetDB().WithContext(ctx)
	if err := db.Find(&users).Error; err != nil {
		return refreshResult, fmt.Errorf("list users for storage usage refresh: %w", err)
	}

	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User:    cfg.Storage.Prefix.User,
		Account: cfg.Storage.Prefix.Account,
		Public:  cfg.Storage.Prefix.Public,
	}

	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return refreshResult, fmt.Errorf("storage usage refresh canceled: %w", err)
		}
		if user.Space == "" {
			klog.Warningf("RefreshUserSpaceSizes: user %q has no storage space path", user.Name)
			refreshResult.Failed++
			continue
		}

		size, err := ceph.GetCephDirectorySize(
			clients.KubeClient, clients.KubeConfig, ceph.StorageQuotaRookNamespace(), "/user/"+user.Space, prefixConfig,
		)
		if err != nil {
			klog.Errorf("RefreshUserSpaceSizes: read usage for user %q: %v", user.Name, err)
			refreshResult.Failed++
			continue
		}

		var userSpaceSize model.UserSpaceSize
		result := db.Where("user_id = ?", user.ID).First(&userSpaceSize)
		switch {
		case errors.Is(result.Error, gorm.ErrRecordNotFound):
			userSpaceSize = model.UserSpaceSize{
				UserID:   user.ID,
				Username: user.Name,
				Size:     size,
			}
			if err := db.Create(&userSpaceSize).Error; err != nil {
				klog.Errorf("RefreshUserSpaceSizes: create usage cache for user %q: %v", user.Name, err)
				refreshResult.Failed++
				continue
			}
		case result.Error != nil:
			klog.Errorf("RefreshUserSpaceSizes: query usage cache for user %q: %v", user.Name, result.Error)
			refreshResult.Failed++
			continue
		default:
			userSpaceSize.Username = user.Name
			userSpaceSize.Size = size
			if err := db.Save(&userSpaceSize).Error; err != nil {
				klog.Errorf("RefreshUserSpaceSizes: update usage cache for user %q: %v", user.Name, err)
				refreshResult.Failed++
				continue
			}
		}

		refreshResult.Updated++
	}

	refreshResult.RefreshedAt = time.Now()
	return refreshResult, nil
}

// RunAnalyzeStorageAlerts 对超过90%理论配额且未临时扩容的用户并发执行 AI 分析，
// 并自动应用决策（冻结作业 / 临时扩容）。
//
//nolint:funlen // Storage alert analysis keeps candidate selection, LLM decision, and enforcement in one cron action.
func RunAnalyzeStorageAlerts(ctx context.Context, clients *Clients) (any, error) {
	db := query.GetDB()
	maxConcurrentStorageAnalysis := storageAnalysisConcurrency()

	// 查询有空间大小记录的用户，附带配额信息
	type userWithUsage struct {
		ID                 uint   `gorm:"column:id"`
		Name               string `gorm:"column:name"`
		Space              string `gorm:"column:space"`
		SpaceQuota         int64  `gorm:"column:space_quota"`
		OriginalSpaceQuota *int64 `gorm:"column:original_space_quota"`
		CurrentSize        int64  `gorm:"column:current_size"`
	}

	var candidates []userWithUsage
	if err := db.Raw(`
		SELECT u.id, u.name, u.space, u.space_quota, u.original_space_quota, uss.size AS current_size
		FROM users u
		JOIN user_space_sizes uss ON uss.user_id = u.id
		WHERE u.deleted_at IS NULL
		  AND u.original_space_quota IS NULL
		  AND u.space_quota > 0
	`).Scan(&candidates).Error; err != nil {
		return nil, fmt.Errorf("查询用户列表失败: %w", err)
	}

	// 过滤出超过 90% 的用户
	var alertUsers []userWithUsage
	for _, u := range candidates {
		if float64(u.CurrentSize)/float64(u.SpaceQuota) >= 0.9 {
			alertUsers = append(alertUsers, u)
		}
	}

	klog.Infof("RunAnalyzeStorageAlerts: %d 个用户超过90%%配额，启动并发 AI 分析（最大并发 %d）",
		len(alertUsers), maxConcurrentStorageAnalysis)

	if len(alertUsers) == 0 {
		return "无超额用户，无需分析", nil
	}

	if clients.StorageAgent == nil {
		if clients.StorageAgentStart == nil || clients.StorageAgentAwait == nil {
			klog.Warningf("RunAnalyzeStorageAlerts: StorageAgent 未注入，跳过 AI 分析")
			return "StorageAgent 未配置", nil
		}
	}

	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User:    cfg.Storage.Prefix.User,
		Account: cfg.Storage.Prefix.Account,
		Public:  cfg.Storage.Prefix.Public,
	}

	applyDecision := func(u userWithUsage, decision *AgentDecision) {
		klog.Infof("RunAnalyzeStorageAlerts: 用户 %s 决策: allow_expand=%v expand_bytes=%d freeze=%v reason=%s",
			u.Name, decision.AllowExpand, decision.ExpandBytes, decision.FreezeNewJobs, decision.Reason)

		recordDecision := func(action string, runErr error) {
			if clients.RecordDecision != nil && decision.DecisionJobID != "" {
				clients.RecordDecision(ctx, decision.DecisionJobID, action, runErr)
			}
		}
		if decision.AllowExpand && decision.ExpandBytes > 0 {
			newQuota := u.SpaceQuota + decision.ExpandBytes
			if err := db.Exec(
				"UPDATE users SET original_space_quota = space_quota, space_quota = ?, jobs_frozen = ? WHERE id = ? AND deleted_at IS NULL",
				newQuota, decision.FreezeNewJobs, u.ID,
			).Error; err != nil {
				klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s 写入扩容失败: %v", u.Name, err)
				recordDecision("expand_failed", err)
				return
			}
			if u.Space != "" {
				if cephErr := ceph.SetCephDirectoryQuota(
					clients.KubeClient, clients.KubeConfig, ceph.StorageQuotaRookNamespace(),
					"/user/"+u.Space, prefixConfig, newQuota,
				); cephErr != nil {
					klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s Ceph 配额同步失败: %v", u.Name, cephErr)
				}
			}
			klog.Infof("RunAnalyzeStorageAlerts: 用户 %s 已临时扩容至 %d bytes", u.Name, newQuota)
			recordDecision("expand", nil)
			return
		}

		if decision.FreezeNewJobs {
			if err := db.Exec(
				"UPDATE users SET jobs_frozen = true WHERE id = ? AND deleted_at IS NULL", u.ID,
			).Error; err != nil {
				klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s 设置 jobs_frozen 失败: %v", u.Name, err)
				recordDecision("freeze_failed", err)
				return
			}

			recordDecision("freeze", nil)
			klog.Infof("RunAnalyzeStorageAlerts: 用户 %s 已冻结新作业创建", u.Name)
			return
		}

		recordDecision("observe", nil)
	}

	if clients.StorageAgentStart != nil && clients.StorageAgentAwait != nil {
		type pendingDecision struct {
			user  userWithUsage
			jobID string
		}

		sem := make(chan struct{}, maxConcurrentStorageAnalysis)
		var wg sync.WaitGroup
		var mu sync.Mutex
		pending := make([]pendingDecision, 0, len(alertUsers))

		for _, candidate := range alertUsers {
			u := candidate
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				klog.Infof("RunAnalyzeStorageAlerts: 开始派发用户 %s 的异步 AI 分析任务", u.Name)
				jobID, err := clients.StorageAgentStart(ctx, u.Name)
				if err != nil {
					klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s AI 分析任务派发失败: %v", u.Name, err)
					return
				}

				klog.Infof("RunAnalyzeStorageAlerts: 用户 %s AI 分析任务已派发 job_id=%s", u.Name, jobID)
				mu.Lock()
				pending = append(pending, pendingDecision{user: u, jobID: jobID})
				mu.Unlock()
			}()
		}
		wg.Wait()

		if len(pending) == 0 {
			return "没有成功派发任何 AI 分析任务", nil
		}

		klog.Infof(
			"RunAnalyzeStorageAlerts: %d 个用户的 AI 分析任务已派发完成，开始并发等待结果（最大并发 %d）",
			len(pending),
			maxConcurrentStorageAnalysis,
		)

		wg = sync.WaitGroup{}
		for _, item := range pending {
			pendingItem := item
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()

				decision, err := clients.StorageAgentAwait(ctx, pendingItem.user.Name, pendingItem.jobID)
				if err != nil {
					klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s 等待 AI 分析结果失败: %v", pendingItem.user.Name, err)
					return
				}

				applyDecision(pendingItem.user, decision)
			}()
		}
		wg.Wait()
		return fmt.Sprintf("分析完成，共处理 %d 个超额用户", len(alertUsers)), nil
	}

	// 使用 channel 实现有界并发
	sem := make(chan struct{}, maxConcurrentStorageAnalysis)
	var wg sync.WaitGroup
	for _, candidate := range alertUsers {
		u := candidate
		wg.Add(1)
		sem <- struct{}{} // 占槽（满时阻塞）
		go func() {
			defer wg.Done()
			defer func() { <-sem }() // 释放槽

			klog.Infof("RunAnalyzeStorageAlerts: 开始分析用户 %s (size=%d quota=%d %.1f%%)",
				u.Name, u.CurrentSize, u.SpaceQuota, float64(u.CurrentSize)/float64(u.SpaceQuota)*100)

			decision, err := clients.StorageAgent(u.Name)
			if err != nil {
				klog.Errorf("RunAnalyzeStorageAlerts: 用户 %s AI 分析失败: %v", u.Name, err)
				return
			}

			applyDecision(u, decision)
		}()
	}

	wg.Wait()
	return fmt.Sprintf("分析完成，共处理 %d 个超额用户", len(alertUsers)), nil
}

// GetPatrolFunc 根据作业名称返回对应的巡检函数
// RunAutoShrinkStorageExpansions automatically recovers temporary storage expansions
// once a user's current usage has fallen below a conservative percentage of the
// original theoretical quota.
func RunAutoShrinkStorageExpansions(ctx context.Context, clients *Clients) (any, error) {
	db := query.GetDB()

	type expandedUser struct {
		ID                   uint       `gorm:"column:id"`
		Name                 string     `gorm:"column:name"`
		Space                string     `gorm:"column:space"`
		SpaceQuota           int64      `gorm:"column:space_quota"`
		OriginalSpaceQuota   int64      `gorm:"column:original_space_quota"`
		CurrentSize          int64      `gorm:"column:current_size"`
		ShrinkStage          string     `gorm:"column:shrink_stage"`
		ShrinkStageUpdatedAt *time.Time `gorm:"column:shrink_stage_updated_at"`
	}

	var users []expandedUser
	if err := db.Raw(`
		SELECT u.id, u.name, u.space, u.space_quota, u.original_space_quota, uss.size AS current_size,
		       u.shrink_stage, u.shrink_stage_updated_at
		FROM users u
		JOIN user_space_sizes uss ON uss.user_id = u.id
		WHERE u.deleted_at IS NULL
		  AND u.original_space_quota IS NOT NULL
	`).Scan(&users).Error; err != nil {
		return nil, fmt.Errorf("query expanded users failed: %w", err)
	}

	if len(users) == 0 {
		return "当前没有处于临时扩容状态的用户，无需执行自动缩容。", nil
	}

	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User:    cfg.Storage.Prefix.User,
		Account: cfg.Storage.Prefix.Account,
		Public:  cfg.Storage.Prefix.Public,
	}

	shrunk := 0
	skipped := 0
	for _, user := range users {
		if user.OriginalSpaceQuota <= 0 {
			skipped++
			continue
		}

		usageRatio := float64(user.CurrentSize) / float64(user.OriginalSpaceQuota)
		stage := user.ShrinkStage
		if stage == "" {
			stage = autoShrinkStageExpanded
		}

		switch stage {
		case autoShrinkStageExpanded:
			if usageRatio >= autoShrinkToBufferThreshold {
				skipped++
				continue
			}

			bufferQuota := calculateShrinkBufferQuota(user.OriginalSpaceQuota, user.SpaceQuota)
			if bufferQuota <= user.OriginalSpaceQuota {
				bufferQuota = user.OriginalSpaceQuota
			}

			if err := db.Exec(
				"UPDATE users SET space_quota = ?, shrink_stage = ?, shrink_stage_updated_at = NOW() WHERE id = ? AND deleted_at IS NULL",
				bufferQuota, autoShrinkStageBuffer, user.ID,
			).Error; err != nil {
				klog.Errorf("RunAutoShrinkStorageExpansions: user=%s buffer shrink failed: %v", user.Name, err)
				skipped++
				continue
			}

			if user.Space != "" {
				if cephErr := ceph.SetCephDirectoryQuota(
					clients.KubeClient,
					clients.KubeConfig,
					ceph.StorageQuotaRookNamespace(),
					"/user/"+user.Space,
					prefixConfig,
					bufferQuota,
				); cephErr != nil {
					klog.Errorf("RunAutoShrinkStorageExpansions: user=%s ceph buffer shrink failed: %v", user.Name, cephErr)
					skipped++
					continue
				}
			}

			shrunk++
			klog.Infof(
				"RunAutoShrinkStorageExpansions: user=%s moved to buffer stage quota=%d current_size=%d ratio=%.2f",
				user.Name,
				bufferQuota,
				user.CurrentSize,
				usageRatio,
			)
		case autoShrinkStageBuffer:
			if user.ShrinkStageUpdatedAt == nil || time.Since(*user.ShrinkStageUpdatedAt) < autoShrinkObservationWindow {
				skipped++
				continue
			}
			if usageRatio >= autoShrinkRecoverThreshold {
				skipped++
				continue
			}

			if err := db.Exec(
				"UPDATE users SET space_quota = ?, original_space_quota = NULL, jobs_frozen = false, shrink_stage = NULL, shrink_stage_updated_at = NULL WHERE id = ? AND deleted_at IS NULL",
				user.OriginalSpaceQuota, user.ID,
			).Error; err != nil {
				klog.Errorf("RunAutoShrinkStorageExpansions: user=%s final shrink failed: %v", user.Name, err)
				skipped++
				continue
			}

			if user.Space != "" {
				if cephErr := ceph.SetCephDirectoryQuota(
					clients.KubeClient,
					clients.KubeConfig,
					ceph.StorageQuotaRookNamespace(),
					"/user/"+user.Space,
					prefixConfig,
					user.OriginalSpaceQuota,
				); cephErr != nil {
					klog.Errorf("RunAutoShrinkStorageExpansions: user=%s ceph final shrink failed: %v", user.Name, cephErr)
					skipped++
					continue
				}
			}

			shrunk++
			klog.Infof(
				"RunAutoShrinkStorageExpansions: user=%s fully restored quota=%d current_size=%d ratio=%.2f",
				user.Name,
				user.OriginalSpaceQuota,
				user.CurrentSize,
				usageRatio,
			)
		default:
			skipped++
		}
	}

	return fmt.Sprintf("自动缩容扫描完成：已处理 %d 个用户，跳过 %d 个用户。", shrunk, skipped), nil
}

func calculateShrinkBufferQuota(originalQuota, currentQuota int64) int64 {
	if currentQuota <= originalQuota {
		return originalQuota
	}

	delta := currentQuota - originalQuota
	bufferQuota := originalQuota + delta/2
	if bufferQuota <= originalQuota {
		return originalQuota
	}
	return bufferQuota
}

func GetPatrolFunc(jobName string, clients *Clients, jobConfig datatypes.JSON) (util.AnyFunc, error) {
	var f util.AnyFunc
	switch jobName {
	case TRIGGER_GPU_ANALYSIS_JOB:
		// TRIGGER_GPU_ANALYSIS_JOB 不需要 req 参数，但为了保持一致性，仍然定义了结构体
		req := &TriggerGpuAnalysisRequest{}
		if len(jobConfig) > 0 {
			if err := json.Unmarshal(jobConfig, req); err != nil {
				return nil, err
			}
		}
		f = func(ctx context.Context) (any, error) {
			return RunTriggerGpuAnalysis(ctx, clients)
		}
	case TRIGGER_BILLING_BASE_LOOP_JOB:
		f = func(ctx context.Context) (any, error) {
			return RunTriggerBillingBaseLoop(ctx, clients)
		}
	default:
		return nil, fmt.Errorf("unsupported patrol job name: %s", jobName)
	}
	return f, nil
}
