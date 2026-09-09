package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	TRIGGER_GPU_ANALYSIS_JOB      = "trigger-gpu-analysis-job"
	TRIGGER_BILLING_BASE_LOOP_JOB = "biling-base-loop"
)

type GpuAnalysisServiceInterface interface {
	TriggerAllJobsAnalysis(ctx context.Context) (int, error)
}

type BillingServiceInterface interface {
	RunBaseLoopOnce(ctx context.Context) (any, error)
}

type Clients struct {
	Client             client.Client
	KubeClient         kubernetes.Interface
	KubeConfig         *rest.Config
	PromClient         monitor.PrometheusInterface
	GpuAnalysisService GpuAnalysisServiceInterface
	BillingService     BillingServiceInterface
}

func NewPatrolClients(
	cli client.Client,
	kubeClient kubernetes.Interface,
	promClient monitor.PrometheusInterface,
	gpuAnalysisService GpuAnalysisServiceInterface,
	billingService BillingServiceInterface,
) *Clients {
	return &Clients{
		Client: cli, KubeClient: kubeClient, PromClient: promClient,
		GpuAnalysisService: gpuAnalysisService, BillingService: billingService,
	}
}

// GetPatrolFunc returns the configured patrol task implementation.
func GetPatrolFunc(jobName string, clients *Clients, jobConfig datatypes.JSON) (util.AnyFunc, error) {
	var f util.AnyFunc
	switch jobName {
	case TRIGGER_GPU_ANALYSIS_JOB:
		req := &TriggerGpuAnalysisRequest{}
		if len(jobConfig) > 0 {
			if err := json.Unmarshal(jobConfig, req); err != nil {
				return nil, err
			}
		}
		f = func(ctx context.Context) (any, error) { return RunTriggerGpuAnalysis(ctx, clients) }
	case TRIGGER_BILLING_BASE_LOOP_JOB:
		f = func(ctx context.Context) (any, error) { return RunTriggerBillingBaseLoop(ctx, clients) }
	default:
		return nil, fmt.Errorf("unsupported patrol job name: %s", jobName)
	}
	return f, nil
}

type StorageUsageRefreshResult struct {
	Updated     int       `json:"updated"`
	Failed      int       `json:"failed"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

// RefreshUserSpaceSizes reconciles the cached usage and database quota mirror
// with values currently enforced by CephFS after an explicit admin request.
func RefreshUserSpaceSizes(ctx context.Context, clients *Clients) (StorageUsageRefreshResult, error) {
	result := StorageUsageRefreshResult{}
	if !ceph.StorageQuotaEnabled() {
		return result, fmt.Errorf("storage quota usage refresh is disabled")
	}
	var users []model.User
	db := query.GetDB().WithContext(ctx)
	if err := db.Find(&users).Error; err != nil {
		return result, fmt.Errorf("list users for storage usage refresh: %w", err)
	}
	prefixes := storagePrefixes()
	for i := range users {
		user := &users[i]
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("storage usage refresh canceled: %w", err)
		}
		if user.Space == "" {
			result.Failed++
			continue
		}
		if err := reconcileUserSpaceSize(ctx, db, clients, prefixes, user.ID); err != nil {
			klog.Errorf("RefreshUserSpaceSizes: reconcile user %q: %v", user.Name, err)
			result.Failed++
			continue
		}
		result.Updated++
	}
	result.RefreshedAt = time.Now()
	return result, nil
}

func reconcileUserSpaceSize(
	ctx context.Context,
	db *gorm.DB,
	clients *Clients,
	prefixes ceph.StoragePrefixConfig,
	userID uint,
) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user model.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
			return err
		}
		logicalPath := "/user/" + user.Space
		size, err := ceph.GetCephDirectorySize(
			clients.KubeClient, clients.KubeConfig, ceph.StorageQuotaRookNamespace(), logicalPath, prefixes,
		)
		if err != nil {
			return fmt.Errorf("read usage: %w", err)
		}
		quota, err := ceph.GetCephDirectoryQuota(
			clients.KubeClient, clients.KubeConfig, ceph.StorageQuotaRookNamespace(), logicalPath, prefixes,
		)
		if err != nil {
			return fmt.Errorf("read quota: %w", err)
		}
		if err := upsertUserSpaceSize(tx, &user, size); err != nil {
			return err
		}
		updated := tx.Model(&model.User{}).Where("id = ?", user.ID).Update("space_quota", quota)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("quota mirror update affected %d rows", updated.RowsAffected)
		}
		return nil
	})
}

func upsertUserSpaceSize(tx *gorm.DB, user *model.User, size int64) error {
	var cached model.UserSpaceSize
	lookup := tx.Where("user_id = ?", user.ID).First(&cached)
	switch {
	case errors.Is(lookup.Error, gorm.ErrRecordNotFound):
		return tx.Create(&model.UserSpaceSize{UserID: user.ID, Username: user.Name, Size: size}).Error
	case lookup.Error != nil:
		return lookup.Error
	default:
		return tx.Model(&cached).Updates(map[string]any{"username": user.Name, "size": size}).Error
	}
}

func storagePrefixes() ceph.StoragePrefixConfig {
	cfg := config.GetConfig()
	return ceph.StoragePrefixConfig{
		User: cfg.Storage.Prefix.User, Account: cfg.Storage.Prefix.Account, Public: cfg.Storage.Prefix.Public,
	}
}
