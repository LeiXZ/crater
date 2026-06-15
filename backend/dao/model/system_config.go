package model

// SystemConfig stores system-wide key-value configuration.
type SystemConfig struct {
	Key   string `gorm:"primarykey;size:100;comment:配置项键"`
	Value string `gorm:"type:text;comment:配置项值"`
}

const (
	// Generic LLM configuration keys.
	ConfigKeyLLMBaseURL   = "LLM_API_BASE_URL" // e.g. https://api.openai.com/v1
	ConfigKeyLLMAPIKey    = "LLM_API_KEY"      // #nosec G101
	ConfigKeyLLMModelName = "LLM_MODEL_NAME"

	// 功能开关配置键
	ConfigKeyEnableGpuAnalysis = "ENABLE_GPU_ANALYSIS" // 值: "true" or "false"

	// Billing 功能与调度配置键
	ConfigKeyEnableBillingFeature                     = "ENABLE_BILLING_FEATURE"
	ConfigKeyEnableBillingActive                      = "ENABLE_BILLING_ACTIVE"
	ConfigKeyEnableRunningSettlement                  = "ENABLE_RUNNING_SETTLEMENT"
	ConfigKeyRunningSettlementIntervalMinute          = "RUNNING_SETTLEMENT_INTERVAL_MINUTES"
	ConfigKeyBillingJobFreeMinutes                    = "BILLING_JOB_FREE_MINUTES"
	ConfigKeyBillingDefaultIssueAmount                = "BILLING_DEFAULT_ISSUE_AMOUNT"
	ConfigKeyBillingDefaultIssuePeriodMinute          = "BILLING_DEFAULT_ISSUE_PERIOD_MINUTES"
	ConfigKeyBillingAccountIssueAmountOverrideEnabled = "ENABLE_BILLING_ACCOUNT_ISSUE_AMOUNT_OVERRIDE"
	ConfigKeyBillingAccountIssuePeriodOverrideEnabled = "ENABLE_BILLING_ACCOUNT_ISSUE_PERIOD_OVERRIDE"

	// 模型与数据集下载额度配置键
	ConfigKeyModelDownloadLimitEnabled           = "MODEL_DOWNLOAD_LIMIT_ENABLED"
	ConfigKeyModelDownloadMaxConcurrent          = "MODEL_DOWNLOAD_MAX_CONCURRENT"
	ConfigKeyModelDownloadWindowHours            = "MODEL_DOWNLOAD_WINDOW_HOURS"
	ConfigKeyModelDownloadMaxSuccessfulDownloads = "MODEL_DOWNLOAD_MAX_SUCCESSFUL_DOWNLOADS"
	ConfigKeyModelDownloadWhitelistUsers         = "MODEL_DOWNLOAD_WHITELIST_USER_IDS"

	// Pod bandwidth limit configuration keys.
	ConfigKeyPodBandwidthEnabled    = "POD_BANDWIDTH_LIMIT_ENABLED"
	ConfigKeyModelDownloadBandwidth = "POD_BANDWIDTH_MODEL_DOWNLOAD"
	ConfigKeyJobIngressBandwidth    = "POD_BANDWIDTH_JOB_INGRESS"
	ConfigKeyJobEgressBandwidth     = "POD_BANDWIDTH_JOB_EGRESS"

	// Storage decision keys.
	ConfigKeyStorageDecisionMode         = "STORAGE_DECISION_MODE"
	ConfigKeyStorageDecisionConfigSource = "STORAGE_DECISION_CONFIG_SOURCE"
	ConfigKeyStorageDirectModelBaseURL   = "STORAGE_DIRECT_MODEL_BASE_URL"
	ConfigKeyStorageDirectModelAPIKey    = "STORAGE_DIRECT_MODEL_API_KEY" // #nosec G101
	ConfigKeyStorageDirectModelName      = "STORAGE_DIRECT_MODEL_NAME"
)

// DefaultConfigKeys defines keys that must exist after startup.
var DefaultConfigKeys = []string{
	ConfigKeyLLMBaseURL,
	ConfigKeyLLMAPIKey,
	ConfigKeyLLMModelName,
	ConfigKeyStorageDecisionMode,
	ConfigKeyStorageDecisionConfigSource,
	ConfigKeyStorageDirectModelBaseURL,
	ConfigKeyStorageDirectModelAPIKey,
	ConfigKeyStorageDirectModelName,
	ConfigKeyEnableGpuAnalysis,
	ConfigKeyEnableBillingFeature,
	ConfigKeyEnableBillingActive,
	ConfigKeyEnableRunningSettlement,
	ConfigKeyRunningSettlementIntervalMinute,
	ConfigKeyBillingJobFreeMinutes,
	ConfigKeyBillingDefaultIssueAmount,
	ConfigKeyBillingDefaultIssuePeriodMinute,
	ConfigKeyBillingAccountIssueAmountOverrideEnabled,
	ConfigKeyBillingAccountIssuePeriodOverrideEnabled,
	ConfigKeyModelDownloadLimitEnabled,
	ConfigKeyModelDownloadMaxConcurrent,
	ConfigKeyModelDownloadWindowHours,
	ConfigKeyModelDownloadMaxSuccessfulDownloads,
	ConfigKeyModelDownloadWhitelistUsers,
	ConfigKeyPodBandwidthEnabled,
	ConfigKeyModelDownloadBandwidth,
	ConfigKeyJobIngressBandwidth,
	ConfigKeyJobEgressBandwidth,
}
