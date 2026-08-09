package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/raids-lab/crater/dao/model"
	"github.com/raids-lab/crater/dao/query"
	"github.com/raids-lab/crater/internal/bizerr"
	"github.com/raids-lab/crater/internal/resputil"
	"github.com/raids-lab/crater/internal/util"
	"github.com/raids-lab/crater/pkg/ceph"
	"github.com/raids-lab/crater/pkg/config"
	"github.com/raids-lab/crater/pkg/constants"
	"github.com/raids-lab/crater/pkg/monitor"
	"github.com/raids-lab/crater/pkg/patrol"
	"github.com/raids-lab/crater/pkg/storagegovernance"
	"github.com/raids-lab/crater/pkg/storagequota"
)

// ---- LLM 任务状态存储 ----

//nolint:gochecknoinits // This is the standard way to register a gin handler.
func init() {
	Registers = append(Registers, NewStorageMgr)
}

type StorageMgr struct {
	name       string
	kubeClient kubernetes.Interface
	kubeConfig *rest.Config
	promClient monitor.PrometheusInterface
}

type StorageCapabilities struct {
	Backend                string   `json:"backend"`
	Configured             bool     `json:"configured"`
	QuotaEnabled           bool     `json:"quota_enabled"`
	PVCName                string   `json:"pvc_name"`
	PVCNamespace           string   `json:"pvc_namespace,omitempty"`
	PVName                 string   `json:"pv_name,omitempty"`
	CSIDriver              string   `json:"csi_driver,omitempty"`
	QuotaProvider          string   `json:"quota_provider"`
	StorageServerAvailable bool     `json:"storage_server_available"`
	ToolboxAvailable       bool     `json:"toolbox_available"`
	UsageReadable          bool     `json:"usage_readable"`
	QuotaReadable          bool     `json:"quota_readable"`
	QuotaWritable          bool     `json:"quota_writable"`
	Reasons                []string `json:"reasons,omitempty"`
}

type SetUserSpaceQuotaRequest struct {
	Quota int64 `json:"quota" binding:"required"`
}

const (
	cephStorageBackend    = "cephfs"
	unknownStorageBackend = "unknown"
)

// AutoScaleRequest 自动扩缩容请求
type AutoScaleRequest struct {
	MinQuota       int64   `json:"min_quota" binding:"required,min=-1"`               // 最小配额，-1 表示无限制
	MaxQuota       int64   `json:"max_quota" binding:"required,min=-1"`               // 最大配额，-1 表示无限制
	ScaleUpRatio   float64 `json:"scale_up_ratio" binding:"required,min=1"`           // 扩容比例，如 1.5 表示扩容到当前使用的 1.5 倍
	ScaleDownRatio float64 `json:"scale_down_ratio" binding:"required,min=0.1,max=1"` // 缩容比例，如 0.8 表示缩容到当前使用的 0.8 倍
}

func NewStorageMgr(conf *RegisterConfig) Manager {
	return &StorageMgr{
		name:       "storage",
		kubeClient: conf.KubeClient,
		kubeConfig: conf.KubeConfig,
		promClient: conf.PrometheusClient,
	}
}

func (mgr *StorageMgr) GetName() string { return mgr.name }

func (mgr *StorageMgr) RegisterPublic(_ *gin.RouterGroup) {}

func (mgr *StorageMgr) RegisterProtected(g *gin.RouterGroup) {
	g.GET("/capabilities", mgr.GetCapabilities)
	g.GET("/dirsize/*path", mgr.GetDirectorySize)
	g.GET("/my-quota", mgr.GetMyQuota)
}

func (mgr *StorageMgr) RegisterAdmin(g *gin.RouterGroup) {
	g.GET("/capabilities", mgr.GetCapabilities)
	g.GET("/user-spaces", mgr.GetAllUserSpaceSizes)
	g.POST("/user-spaces/refresh", mgr.RefreshUserSpaceSizes)
	g.PUT("/user-spaces/:user/quota", mgr.SetUserSpaceQuota)
}

// GetCapabilities godoc
//
// @Summary Get storage quota capabilities
// @Description Detect whether the configured storage supports CephFS usage and quota operations
// @Tags Storage
// @Produce json
// @Security Bearer
// @Success 200 {object} resputil.Response[StorageCapabilities] "Success"
// @Router /v1/storage/capabilities [get]
// @Router /v1/admin/storage/capabilities [get]
func (mgr *StorageMgr) GetCapabilities(c *gin.Context) {
	resputil.Success(c, mgr.detectCapabilities())
}

func (mgr *StorageMgr) detectCapabilities() StorageCapabilities {
	cfg := config.GetConfig()
	capability := StorageCapabilities{
		Backend:       unknownStorageBackend,
		Configured:    strings.TrimSpace(cfg.Storage.PVC.ReadWriteMany) != "",
		QuotaEnabled:  ceph.StorageQuotaEnabled(),
		PVCName:       strings.TrimSpace(cfg.Storage.PVC.ReadWriteMany),
		QuotaProvider: ceph.StorageQuotaProvider(),
	}
	if !capability.QuotaEnabled {
		capability.Reasons = append(capability.Reasons, "storage quota management is disabled")
		return capability
	}
	if !capability.Configured {
		capability.Reasons = append(capability.Reasons, "storage.pvc.readWriteMany is not configured")
		return capability
	}
	if mgr.kubeClient == nil {
		capability.Reasons = append(capability.Reasons, "kubernetes client is not available")
		return capability
	}

	pvName, pvcNamespace, driver, err := mgr.detectStoragePV(capability.PVCName)
	if err != nil {
		capability.Reasons = append(capability.Reasons, err.Error())
		return capability
	}
	capability.PVName = pvName
	capability.PVCNamespace = pvcNamespace
	capability.CSIDriver = driver

	expectedDriver := ceph.StorageQuotaCephFSCSIDriver()
	if driver != expectedDriver {
		if driver != "" {
			capability.Backend = driver
		}
		capability.Reasons = append(capability.Reasons, fmt.Sprintf(
			"storage PVC CSI driver %q does not match configured CephFS driver %q",
			driver,
			expectedDriver,
		))
		return capability
	}
	capability.Backend = cephStorageBackend

	if capability.QuotaProvider == storagequota.ProviderDisabled {
		capability.Reasons = append(capability.Reasons, "storage quota provider is disabled")
		return capability
	}

	if capability.QuotaProvider != storagequota.ProviderToolbox {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		storageServerCapabilities, storageServerErr := ceph.GetStorageServerQuotaCapabilities(ctx)
		cancel()
		capability.StorageServerAvailable = storageServerErr == nil
		if storageServerErr != nil {
			capability.Reasons = append(
				capability.Reasons,
				fmt.Sprintf("storage-server is not available: %v", storageServerErr),
			)
		} else {
			capability.UsageReadable = storageServerCapabilities.UsageReadable
			capability.QuotaReadable = storageServerCapabilities.QuotaReadable
			capability.QuotaWritable = storageServerCapabilities.QuotaWritable
			capability.Reasons = append(capability.Reasons, storageServerCapabilities.Reasons...)
		}
	}

	needsToolbox := capability.QuotaProvider == storagequota.ProviderToolbox ||
		(capability.QuotaProvider == storagequota.ProviderAuto &&
			(!capability.UsageReadable || !capability.QuotaReadable || !capability.QuotaWritable))
	if needsToolbox {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		toolboxCapabilities, toolboxErr := ceph.GetToolboxQuotaCapabilities(
			ctx,
			mgr.kubeClient,
			mgr.kubeConfig,
			ceph.StorageQuotaRookNamespace(),
		)
		cancel()
		capability.ToolboxAvailable = toolboxErr == nil && toolboxCapabilities.UsageReadable
		if toolboxErr != nil {
			capability.Reasons = append(
				capability.Reasons,
				fmt.Sprintf("toolbox is not available: %v", toolboxErr),
			)
		} else {
			capability.UsageReadable = capability.UsageReadable || toolboxCapabilities.UsageReadable
			capability.QuotaReadable = capability.QuotaReadable || toolboxCapabilities.QuotaReadable
			capability.QuotaWritable = capability.QuotaWritable || toolboxCapabilities.QuotaWritable
			capability.Reasons = append(capability.Reasons, toolboxCapabilities.Reasons...)
		}
	}
	return capability
}

func (mgr *StorageMgr) detectStoragePV(pvcName string) (string, string, string, error) {
	cfg := config.GetConfig()
	pvcNamespace := strings.TrimSpace(cfg.Namespaces.Job)
	if pvcNamespace == "" {
		return "", "", "", fmt.Errorf("job namespace is not configured")
	}
	pvc, err := mgr.kubeClient.CoreV1().PersistentVolumeClaims(pvcNamespace).
		Get(context.TODO(), pvcName, metav1.GetOptions{})
	if err != nil {
		return "", pvcNamespace, "", fmt.Errorf("get storage PVC %s/%s failed: %w", pvcNamespace, pvcName, err)
	}

	if pvc.Spec.VolumeName == "" {
		return "", pvc.Namespace, "", fmt.Errorf("storage PVC %s is not bound to a PV", pvcName)
	}

	pv, err := mgr.kubeClient.CoreV1().PersistentVolumes().Get(context.TODO(), pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return pvc.Spec.VolumeName, pvc.Namespace, "", fmt.Errorf("get storage PV %s failed: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.CSI == nil {
		return pv.Name, pvc.Namespace, "", nil
	}
	return pv.Name, pvc.Namespace, pv.Spec.CSI.Driver, nil
}

// GetDirectorySize godoc
//
// @Summary Get directory size in CephFS
// @Description Get the size of a directory in CephFS using getfattr command
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param path path string true "Directory path"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// @Router /v1/storage/dirsize/{path} [get]
func (mgr *StorageMgr) GetDirectorySize(c *gin.Context) {
	// 1. 获取路径参数
	path := strings.TrimPrefix(c.Request.URL.Path, "/api/v1/storage/dirsize/")
	if path == "" {
		resputil.BadRequestError(c, "路径不能为空")
		return
	}

	// 2. 确保路径以 / 开头
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// 3. 执行 Ceph 命令获取目录大小
	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User:    cfg.Storage.Prefix.User,
		Account: cfg.Storage.Prefix.Account,
		Public:  cfg.Storage.Prefix.Public,
	}
	size, err := ceph.GetCephDirectorySize(
		mgr.kubeClient, mgr.kubeConfig, ceph.StorageQuotaRookNamespace(), path, prefixConfig,
	)
	if err != nil {
		klog.Warningf("GetDirectorySize: failed to get size for %q, returning unknown sentinel: %v", path, err)
		size = -1
	}

	// 4. 返回结果
	resputil.Success(c, gin.H{
		"path":      path,
		"size":      size,
		"unit":      "bytes",
		"formatted": formatSize(size),
	})
}

// GetMyQuota godoc
//
// @Summary Get current user's storage quota
// @Description Get the storage quota for the currently authenticated user
// @Tags Storage
// @Produce json
// @Security Bearer
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// @Router /v1/storage/my-quota [get]
func (mgr *StorageMgr) GetMyQuota(c *gin.Context) {
	token := util.GetToken(c)

	var row struct {
		SpaceQuota int64 `gorm:"column:space_quota"`
	}
	if err := query.GetDB().Raw(
		"SELECT space_quota FROM users WHERE id = ? AND deleted_at IS NULL", token.UserID,
	).Scan(&row).Error; err != nil {
		resputil.Error(c, fmt.Sprintf("获取配额失败: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, gin.H{
		"space_quota":           row.SpaceQuota,
		"space_quota_formatted": formatSize(row.SpaceQuota),
	})
}

// GetAllUserSpaceSizes godoc
//
// @Summary Get all user space sizes
// @Description Get the size of all user spaces from database
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param page query int false "Page number"
// @Param pageSize query int false "Page size"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// @Router /v1/admin/storage/user-spaces [get]
func (mgr *StorageMgr) GetAllUserSpaceSizes(c *gin.Context) {
	// 1. 获取分页参数
	page, pageErr := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, pageSizeErr := strconv.Atoi(c.DefaultQuery("pageSize", "10"))
	if pageErr != nil || page < 1 || pageSizeErr != nil || pageSize < 1 || pageSize > 1000 {
		resputil.BadRequestError(c, "page must be positive and pageSize must be between 1 and 1000")
		return
	}

	// 2. 从数据库中获取用户空间大小和配额
	type UserSpaceInfo struct {
		Username           string     `json:"username"`
		Size               int64      `json:"size"`
		UpdatedAt          *time.Time `json:"updated_at"`
		SpaceQuota         int64      `json:"space_quota"`
		OriginalSpaceQuota *int64     `json:"original_space_quota"`
		JobsFrozen         bool       `json:"jobs_frozen"`
		ShrinkStage        string     `json:"shrink_stage"`
	}

	var userSpaceInfos []UserSpaceInfo
	var total int64

	db := query.GetDB()

	// 计算总数
	if err := db.Model(&model.User{}).Where("deleted_at IS NULL").Count(&total).Error; err != nil {
		klog.Errorf("GetAllUserSpaceSizes: count users: %v", err)
		resputil.Error(c, "Failed to query user storage usage", resputil.ServiceError)
		return
	}

	// 计算分页偏移量
	offset := (page - 1) * pageSize

	// 获取分页数据，关联 User 表获取 SpaceQuota 和 OriginalSpaceQuota
	if err := db.Table("users").
		Select(
			"users.name as username, " +
				"COALESCE(user_space_sizes.size, -1) as size, " +
				"user_space_sizes.updated_at as updated_at, " +
				"users.space_quota as space_quota, " +
				"users.original_space_quota as original_space_quota, " +
				"users.jobs_frozen as jobs_frozen, " +
				"users.shrink_stage as shrink_stage",
		).
		Joins("LEFT JOIN user_space_sizes ON user_space_sizes.user_id = users.id").
		Where("users.deleted_at IS NULL").
		Order("users.id ASC").
		Offset(offset).Limit(pageSize).
		Find(&userSpaceInfos).Error; err != nil {
		klog.Errorf("GetAllUserSpaceSizes: query user storage usage: %v", err)
		resputil.Error(c, "Failed to query user storage usage", resputil.ServiceError)
		return
	}

	// 3. 格式化结果
	formattedUserSpaces := make([]map[string]any, 0, len(userSpaceInfos))
	for i := range userSpaceInfos {
		info := userSpaceInfos[i]
		item := map[string]any{
			"user":            info.Username,
			"size":            info.Size,
			"quota":           info.SpaceQuota,
			"unit":            "bytes",
			"formatted":       formatSize(info.Size),
			"updated_at":      info.UpdatedAt,
			"quota_formatted": formatSize(info.SpaceQuota),
			"is_expanded":     info.OriginalSpaceQuota != nil,
			"jobs_frozen":     info.JobsFrozen,
			"shrink_stage":    info.ShrinkStage,
		}
		if info.OriginalSpaceQuota != nil {
			item["original_quota"] = *info.OriginalSpaceQuota
			item["original_quota_formatted"] = formatSize(*info.OriginalSpaceQuota)
		}
		formattedUserSpaces = append(formattedUserSpaces, item)
	}

	// 4. 返回结果（包含分页信息）
	resputil.Success(c, gin.H{
		"items":      formattedUserSpaces,
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": (int(total) + pageSize - 1) / pageSize,
	})
}

// RefreshUserSpaceSizes godoc
//
// @Summary Refresh all user space usage
// @Description Read current CephFS usage for every user directory and update the usage cache
// @Tags Storage
// @Produce json
// @Security Bearer
// @Success 200 {object} resputil.Response[patrol.StorageUsageRefreshResult] "Success"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// @Router /v1/admin/storage/user-spaces/refresh [post]
func (mgr *StorageMgr) RefreshUserSpaceSizes(c *gin.Context) {
	result, err := patrol.RefreshUserSpaceSizes(c.Request.Context(), &patrol.Clients{
		KubeClient: mgr.kubeClient,
		KubeConfig: mgr.kubeConfig,
	})
	if err != nil {
		resputil.HandleError(c, bizerr.Internal.FileSystemError.Wrap(err, "failed to refresh storage usage"))
		return
	}

	resputil.Success(c, result)
}

// SetUserSpaceQuota godoc
//
// @Summary Set user space quota
// @Description Set the space quota for a user
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Param quota body SetUserSpaceQuotaRequest true "Space quota request"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 404 {object} resputil.Response[any] "User not found"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// @Router /v1/admin/storage/user-spaces/{user}/quota [put]
func (mgr *StorageMgr) SetUserSpaceQuota(c *gin.Context) {
	// 1. 获取用户名参数
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "username is required")
		return
	}

	// 2. 解析请求体
	var req SetUserSpaceQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resputil.BadRequestError(c, "quota must be provided as an integer number of bytes")
		return
	}

	// 3. 验证配额值
	if req.Quota < -1 || req.Quota == 0 {
		resputil.BadRequestError(c, "quota must be -1 for unlimited or greater than zero")
		return
	}

	// 4. 获取用户信息（含临时扩容状态）
	db := query.GetDB()
	var userRow struct {
		model.User
		SpaceQuota         int64  `gorm:"column:space_quota"`
		OriginalSpaceQuota *int64 `gorm:"column:original_space_quota"`
	}
	if err := db.Model(&model.User{}).
		Select("users.*, users.space_quota, users.original_space_quota").
		Where("name = ?", user).
		First(&userRow).Error; err != nil {
		resputil.Error(c, "User was not found", resputil.NotSpecified)
		return
	}
	userInfo := userRow.User
	auditDetails := map[string]any{
		"old_quota": userRow.SpaceQuota,
		"new_quota": req.Quota,
		"provider":  ceph.StorageQuotaProvider(),
	}

	// A manual change overrides any legacy temporary expansion. Apply CephFS
	// first so the database never reports a quota that was not enforced.
	if userRow.OriginalSpaceQuota != nil {
		auditDetails["old_original_quota"] = *userRow.OriginalSpaceQuota
		auditDetails["cleared_temporary_expansion"] = true
	}

	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User: cfg.Storage.Prefix.User, Account: cfg.Storage.Prefix.Account, Public: cfg.Storage.Prefix.Public,
	}
	userPath := fmt.Sprintf("/user/%s", userInfo.Space)
	if err := ceph.SetCephDirectoryQuota(
		mgr.kubeClient, mgr.kubeConfig, ceph.StorageQuotaRookNamespace(), userPath, prefixConfig, req.Quota,
	); err != nil {
		klog.Errorf("SetUserSpaceQuota: set CephFS quota for user %q: %v", user, err)
		auditDetails["ceph_applied"] = false
		RecordOperationLog(c, constants.OpTypeSetStorageQuota, user, constants.OpStatusFailed, err.Error(), auditDetails)
		resputil.Error(c, "Failed to apply the CephFS storage quota", resputil.ServiceError)
		return
	}
	auditDetails["ceph_applied"] = true
	if err := db.Model(&model.User{}).Where("name = ?", user).Updates(map[string]any{
		"space_quota":             req.Quota,
		"original_space_quota":    nil,
		"jobs_frozen":             false,
		"shrink_stage":            nil,
		"shrink_stage_updated_at": nil,
	}).Error; err != nil {
		rollbackErr := ceph.SetCephDirectoryQuota(
			mgr.kubeClient, mgr.kubeConfig, ceph.StorageQuotaRookNamespace(), userPath, prefixConfig, userRow.SpaceQuota,
		)
		klog.Errorf("SetUserSpaceQuota: update database quota for user %q: %v; CephFS rollback: %v", user, err, rollbackErr)
		auditDetails["rollback_succeeded"] = rollbackErr == nil
		RecordOperationLog(c, constants.OpTypeSetStorageQuota, user, constants.OpStatusFailed, err.Error(), auditDetails)
		resputil.Error(c, "Failed to save the storage quota", resputil.ServiceError)
		return
	}

	RecordOperationLog(c, constants.OpTypeSetStorageQuota, user, constants.OpStatusSuccess, "", auditDetails)

	resputil.Success(c, gin.H{
		"user":            user,
		"quota":           req.Quota,
		"unit":            "bytes",
		"quota_formatted": formatSize(req.Quota),
		"ceph_quota_set":  true,
	})
}

// AutoScaleUserSpaceQuota godoc
//
// @Summary Auto scale user space quota
// @Description Auto scale the space quota for a user based on current usage
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Param body body AutoScaleRequest true "Auto scale configuration"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 404 {object} resputil.Response[any] "User not found"
// @Failure 500 {object} resputil.Response[any] "Other errors"
func (mgr *StorageMgr) AutoScaleUserSpaceQuota(c *gin.Context) {
	// 1. 获取用户名参数
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	// 2. 解析请求体
	var req AutoScaleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resputil.BadRequestError(c, "请求体格式错误: "+err.Error())
		return
	}

	// 3. 获取用户信息和当前使用空间大小
	db := query.GetDB()
	var userInfo model.User
	if err := db.Where("name = ?", user).First(&userInfo).Error; err != nil {
		resputil.Error(c, "用户不存在", resputil.NotSpecified)
		return
	}

	var userSpaceSize model.UserSpaceSize
	if err := db.Where("user_id = ?", userInfo.ID).First(&userSpaceSize).Error; err != nil {
		resputil.Error(c, fmt.Sprintf("获取用户空间使用情况失败: %v", err), resputil.NotSpecified)
		return
	}

	// 4. 计算新的配额
	currentUsage := userSpaceSize.Size
	newQuota := int64(float64(currentUsage) * req.ScaleUpRatio)

	// 应用最小和最大配额限制
	if req.MinQuota != -1 && newQuota < req.MinQuota {
		newQuota = req.MinQuota
	}
	if req.MaxQuota != -1 && newQuota > req.MaxQuota {
		newQuota = req.MaxQuota
	}

	// 5. 更新用户配额
	if err := db.Model(&model.User{}).Where("name = ?", user).Update("space_quota", newQuota).Error; err != nil {
		resputil.Error(c, fmt.Sprintf("更新用户配额失败: %v", err), resputil.NotSpecified)
		return
	}

	// 6. 实际设置 CephFS 目录配额
	cfg := config.GetConfig()
	prefixConfig := ceph.StoragePrefixConfig{
		User:    cfg.Storage.Prefix.User,
		Account: cfg.Storage.Prefix.Account,
		Public:  cfg.Storage.Prefix.Public,
	}

	// 构建用户空间路径
	userPath := fmt.Sprintf("/user/%s", userInfo.Space)

	// 调用 SetCephDirectoryQuota 设置实际配额
	cephErr := ceph.SetCephDirectoryQuota(
		mgr.kubeClient, mgr.kubeConfig, ceph.StorageQuotaRookNamespace(), userPath, prefixConfig, newQuota,
	)
	if cephErr != nil {
		// 记录错误但不影响响应，确保数据库更新成功
		klog.Errorf("AutoScaleUserSpaceQuota: 设置用户 %s Ceph 配额失败: %v", user, cephErr)
	}

	// 7. 返回结果
	resputil.Success(c, gin.H{
		"user":                    user,
		"current_usage":           currentUsage,
		"new_quota":               newQuota,
		"unit":                    "bytes",
		"current_usage_formatted": formatSize(currentUsage),
		"new_quota_formatted":     formatSize(newQuota),
		"ceph_quota_set":          cephErr == nil,
		"ceph_quota_error":        cephErr,
	})
}

// RunAutoShrink triggers one manual scan that shrinks users currently in temporary
// expansion state back to their original quota when it is safe to do so.
func (mgr *StorageMgr) RunAutoShrink(c *gin.Context) {
	result, err := patrol.RunAutoShrinkStorageExpansions(c.Request.Context(), &patrol.Clients{
		KubeClient: mgr.kubeClient,
		KubeConfig: mgr.kubeConfig,
		PromClient: mgr.promClient,
	})
	if err != nil {
		resputil.Error(c, fmt.Sprintf("自动缩容执行失败：%v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, gin.H{
		"message": result,
	})
}

// ApplyExpansion godoc
//
// @Summary Apply temporary storage expansion for a user
// @Description Save the current quota as original and set an expanded quota
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Param body body object true "expand_bytes: bytes to add on top of current quota"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 500 {object} resputil.Response[any] "Other errors"
func (mgr *StorageMgr) ApplyExpansion(c *gin.Context) {
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	var req struct {
		ExpandBytes   int64  `json:"expand_bytes" binding:"required,min=1"`
		FreezeNewJobs bool   `json:"freeze_new_jobs"`
		DecisionJobID string `json:"decision_job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		resputil.BadRequestError(c, "请求体格式错误: "+err.Error())
		return
	}

	db := query.GetDB()

	// 查询当前配额和原始配额
	var row struct {
		SpaceQuota         int64  `gorm:"column:space_quota"`
		OriginalSpaceQuota *int64 `gorm:"column:original_space_quota"`
	}
	if err := db.Raw(
		"SELECT space_quota, original_space_quota FROM users WHERE name = ? AND deleted_at IS NULL",
		user,
	).Scan(&row).Error; err != nil {
		resputil.Error(c, "用户不存在", resputil.NotSpecified)
		return
	}

	if row.OriginalSpaceQuota != nil {
		resputil.BadRequestError(c, "该用户已存在临时扩容，请先还原后再扩容")
		return
	}

	newQuota := row.SpaceQuota + req.ExpandBytes

	// 保存原始配额，并更新为新配额，同时设置 jobs_frozen
	if err := db.Exec(
		"UPDATE users "+
			"SET original_space_quota = space_quota, space_quota = ?, jobs_frozen = ?, "+
			"shrink_stage = ?, shrink_stage_updated_at = NOW() "+
			"WHERE name = ? AND deleted_at IS NULL",
		newQuota, req.FreezeNewJobs, "expanded", user,
	).Error; err != nil {
		resputil.Error(c, fmt.Sprintf("更新配额失败: %v", err), resputil.NotSpecified)
		return
	}

	// 同步到 CephFS
	var userInfo model.User
	if err := db.Where("name = ?", user).First(&userInfo).Error; err == nil {
		cfg := config.GetConfig()
		prefixConfig := ceph.StoragePrefixConfig{
			User:    cfg.Storage.Prefix.User,
			Account: cfg.Storage.Prefix.Account,
			Public:  cfg.Storage.Prefix.Public,
		}
		if cephErr := ceph.SetCephDirectoryQuota(
			mgr.kubeClient,
			mgr.kubeConfig,
			ceph.StorageQuotaRookNamespace(),
			fmt.Sprintf("/user/%s", userInfo.Space),
			prefixConfig,
			newQuota,
		); cephErr != nil {
			klog.Errorf("ApplyExpansion: 设置用户 %s Ceph 配额失败: %v", user, cephErr)
		}
	}
	if req.DecisionJobID != "" {
		action := "manual_expand"
		if req.FreezeNewJobs {
			action = "manual_expand_and_freeze"
		}
		_ = storagegovernance.MarkDecisionExecution(c.Request.Context(), req.DecisionJobID, action, nil)
	}

	resputil.Success(c, gin.H{
		"user":                     user,
		"original_quota":           row.SpaceQuota,
		"new_quota":                newQuota,
		"original_quota_formatted": formatSize(row.SpaceQuota),
		"new_quota_formatted":      formatSize(newQuota),
		"jobs_frozen":              req.FreezeNewJobs,
	})
}

// RevertExpansion godoc
//
// @Summary Revert temporary storage expansion for a user
// @Description Restore the user's quota to the original value before expansion
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 500 {object} resputil.Response[any] "Other errors"
func (mgr *StorageMgr) RevertExpansion(c *gin.Context) {
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	db := query.GetDB()

	var row struct {
		SpaceQuota         int64  `gorm:"column:space_quota"`
		OriginalSpaceQuota *int64 `gorm:"column:original_space_quota"`
	}
	if err := db.Raw(
		"SELECT space_quota, original_space_quota FROM users WHERE name = ? AND deleted_at IS NULL",
		user,
	).Scan(&row).Error; err != nil {
		resputil.Error(c, "用户不存在", resputil.NotSpecified)
		return
	}

	if row.OriginalSpaceQuota == nil {
		resputil.BadRequestError(c, "该用户当前没有临时扩容，无需还原")
		return
	}

	originalQuota := *row.OriginalSpaceQuota

	// 查询用户 ID 和当前实际用量，决定是否同时解冻
	var userIDRow struct {
		ID uint `gorm:"column:id"`
	}
	db.Raw("SELECT id FROM users WHERE name = ? AND deleted_at IS NULL", user).Scan(&userIDRow)

	var currentSize int64
	var spaceSize model.UserSpaceSize
	if err := db.Where("user_id = ?", userIDRow.ID).First(&spaceSize).Error; err == nil {
		currentSize = spaceSize.Size
	}

	// 只有还原后的理论配额大于当前用量时才自动解冻；否则保持冻结状态
	shouldUnfreeze := originalQuota <= 0 || currentSize < originalQuota
	if shouldUnfreeze {
		if err := db.Exec(
			"UPDATE users "+
				"SET space_quota = ?, original_space_quota = NULL, jobs_frozen = false, "+
				"shrink_stage = NULL, shrink_stage_updated_at = NULL "+
				"WHERE name = ? AND deleted_at IS NULL",
			originalQuota,
			user,
		).Error; err != nil {
			resputil.Error(c, fmt.Sprintf("还原配额失败: %v", err), resputil.NotSpecified)
			return
		}
	} else {
		// 仅还原配额，不解冻（用量仍超出理论配额）
		if err := db.Exec(
			"UPDATE users "+
				"SET space_quota = ?, original_space_quota = NULL, "+
				"shrink_stage = NULL, shrink_stage_updated_at = NULL "+
				"WHERE name = ? AND deleted_at IS NULL",
			originalQuota,
			user,
		).Error; err != nil {
			resputil.Error(c, fmt.Sprintf("还原配额失败: %v", err), resputil.NotSpecified)
			return
		}
	}

	// 同步到 CephFS
	var userInfo model.User
	if err := db.Where("name = ?", user).First(&userInfo).Error; err == nil {
		cfg := config.GetConfig()
		prefixConfig := ceph.StoragePrefixConfig{
			User:    cfg.Storage.Prefix.User,
			Account: cfg.Storage.Prefix.Account,
			Public:  cfg.Storage.Prefix.Public,
		}
		if cephErr := ceph.SetCephDirectoryQuota(
			mgr.kubeClient,
			mgr.kubeConfig,
			ceph.StorageQuotaRookNamespace(),
			fmt.Sprintf("/user/%s", userInfo.Space),
			prefixConfig,
			originalQuota,
		); cephErr != nil {
			klog.Errorf("RevertExpansion: 设置用户 %s Ceph 配额失败: %v", user, cephErr)
		}
	}

	resputil.Success(c, gin.H{
		"user":                     user,
		"reverted_quota":           originalQuota,
		"reverted_quota_formatted": formatSize(originalQuota),
		"jobs_unfrozen":            shouldUnfreeze,
	})
}

// UnfreezeJobs godoc
//
// @Summary Manually unfreeze job creation for a user
// @Description Clear the jobs_frozen flag, allowing the user to create new jobs again
// @Tags Storage
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 400 {object} resputil.Response[any] "Request parameter error"
// @Failure 500 {object} resputil.Response[any] "Other errors"
func (mgr *StorageMgr) UnfreezeJobs(c *gin.Context) {
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	db := query.GetDB()
	if err := db.Exec("UPDATE users SET jobs_frozen = false WHERE name = ? AND deleted_at IS NULL", user).Error; err != nil {
		resputil.Error(c, fmt.Sprintf("解冻失败: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, gin.H{"user": user, "jobs_frozen": false})
}

// FreezeJobs manually freezes job creation for a user and optionally binds the action to a decision record.
func (mgr *StorageMgr) FreezeJobs(c *gin.Context) {
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	var req struct {
		DecisionJobID string `json:"decision_job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		resputil.BadRequestError(c, "请求体格式错误: "+err.Error())
		return
	}

	db := query.GetDB()
	if err := db.Exec("UPDATE users SET jobs_frozen = true WHERE name = ? AND deleted_at IS NULL", user).Error; err != nil {
		if req.DecisionJobID != "" {
			_ = storagegovernance.MarkDecisionExecution(c.Request.Context(), req.DecisionJobID, "manual_freeze_failed", err)
		}
		resputil.Error(c, fmt.Sprintf("冻结失败: %v", err), resputil.NotSpecified)
		return
	}

	if req.DecisionJobID != "" {
		_ = storagegovernance.MarkDecisionExecution(c.Request.Context(), req.DecisionJobID, "manual_freeze", nil)
	}

	resputil.Success(c, gin.H{"user": user, "jobs_frozen": true})
}

// TriggerLLMDecision godoc
//
// @Summary Trigger LLM storage expansion decision for a user
// @Description Calls Claude agent to analyze whether a user needs temporary storage expansion
// @Tags Storage
// @Accept json
// @Produce json
// @Security Bearer
// @Param user path string true "Username"
// @Success 200 {object} resputil.Response[any] "Success"
// @Failure 500 {object} resputil.Response[any] "Other errors"
// TriggerLLMDecision 异步启动 LLM 分析，立即返回 job_id
func (mgr *StorageMgr) TriggerLLMDecision(c *gin.Context) {
	user := c.Param("user")
	if user == "" {
		resputil.BadRequestError(c, "用户名不能为空")
		return
	}

	engine := storagegovernance.NewEngine(
		mgr.kubeClient,
		mgr.kubeConfig,
		mgr.promClient,
		storagegovernance.DefaultConstraintConfig(),
	)
	jobID, err := engine.StartAsyncDecision(context.Background(), storagegovernance.DecisionRequest{
		Username:      user,
		Source:        model.StorageDecisionSourceManual,
		TriggerReason: "manual llm decision request",
	})
	if err != nil {
		klog.Errorf("TriggerLLMDecision: user=%s err=%v", user, err)
		resputil.Error(c, fmt.Sprintf("鍚姩 LLM 鍒嗘瀽澶辫触: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, gin.H{"job_id": jobID})
}

// GetLLMDecisionStatus 查询 LLM 分析任务状态
func (mgr *StorageMgr) GetLLMDecisionStatus(c *gin.Context) {
	jobID := c.Param("job_id")

	job, err := storagegovernance.GetDecisionStatus(c.Request.Context(), jobID)

	if err != nil {
		resputil.Error(c, "任务不存在", resputil.NotSpecified)
		return
	}

	resputil.Success(c, job)
}

// formatSize 格式化大小为人类可读格式
// ListStorageDecisions returns paginated persisted storage decision records.
func (mgr *StorageMgr) ListStorageDecisions(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))

	result, err := storagegovernance.ListDecisionRecords(
		c.Request.Context(),
		page,
		pageSize,
		c.Query("user"),
		c.Query("status"),
		c.Query("source"),
	)
	if err != nil {
		resputil.Error(c, fmt.Sprintf("failed to list storage decisions: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, result)
}

// GetStorageDecision returns one persisted storage decision record with full details.
func (mgr *StorageMgr) GetStorageDecision(c *gin.Context) {
	jobID := c.Param("job_id")
	if jobID == "" {
		resputil.BadRequestError(c, "job_id cannot be empty")
		return
	}

	result, err := storagegovernance.GetDecisionRecord(c.Request.Context(), jobID)
	if err != nil {
		resputil.Error(c, fmt.Sprintf("failed to get storage decision: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, result)
}

// ReplayStorageDecisions re-evaluates stored decisions under the current or overridden safety policy.
func (mgr *StorageMgr) ReplayStorageDecisions(c *gin.Context) {
	var req struct {
		Limit                    int      `json:"limit"`
		MaxExpandRatio           *float64 `json:"max_expand_ratio"`
		MaxExpandBytes           *int64   `json:"max_expand_bytes"`
		MinPlatformReservedRatio *float64 `json:"min_platform_reserved_ratio"`
		MinPlatformReservedBytes *int64   `json:"min_platform_reserved_bytes"`
		ExpansionCooldownHours   *int     `json:"expansion_cooldown_hours"`
		ForceFreezeWhenOverQuota *bool    `json:"force_freeze_when_over_quota"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		resputil.BadRequestError(c, "invalid replay request: "+err.Error())
		return
	}

	cfg := storagegovernance.DefaultConstraintConfig()
	if req.MaxExpandRatio != nil {
		cfg.MaxExpandRatio = *req.MaxExpandRatio
	}
	if req.MaxExpandBytes != nil {
		cfg.MaxExpandBytes = *req.MaxExpandBytes
	}
	if req.MinPlatformReservedRatio != nil {
		cfg.MinPlatformReservedRatio = *req.MinPlatformReservedRatio
	}
	if req.MinPlatformReservedBytes != nil {
		cfg.MinPlatformReservedBytes = *req.MinPlatformReservedBytes
	}
	if req.ExpansionCooldownHours != nil {
		cfg.ExpansionCooldown = time.Duration(*req.ExpansionCooldownHours) * time.Hour
	}
	if req.ForceFreezeWhenOverQuota != nil {
		cfg.ForceFreezeWhenOverQuota = *req.ForceFreezeWhenOverQuota
	}

	summary, err := storagegovernance.ReplayStoredDecisions(c.Request.Context(), cfg, req.Limit)
	if err != nil {
		resputil.Error(c, fmt.Sprintf("failed to replay storage decisions: %v", err), resputil.NotSpecified)
		return
	}

	resputil.Success(c, summary)
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < 0 {
		return "Unknown"
	}
	if bytes == 0 {
		return "0 B"
	}
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
