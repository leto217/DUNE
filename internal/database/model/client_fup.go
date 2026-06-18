package model

// ClientFupState tracks per-period usage baselines and fair-usage enforcement
// state for a client. One row per email; limits live on ClientRecord.
type ClientFupState struct {
	Email string `json:"email" gorm:"primaryKey"`

	DailyBaseline   int64 `json:"dailyBaseline" gorm:"column:daily_baseline;default:0"`
	WeeklyBaseline  int64 `json:"weeklyBaseline" gorm:"column:weekly_baseline;default:0"`
	MonthlyBaseline int64 `json:"monthlyBaseline" gorm:"column:monthly_baseline;default:0"`

	DailyPeriodStart   int64 `json:"dailyPeriodStart" gorm:"column:daily_period_start;default:0"`
	WeeklyPeriodStart  int64 `json:"weeklyPeriodStart" gorm:"column:weekly_period_start;default:0"`
	MonthlyPeriodStart int64 `json:"monthlyPeriodStart" gorm:"column:monthly_period_start;default:0"`

	FupDisabledUntil int64  `json:"fupDisabledUntil" gorm:"column:fup_disabled_until;default:0"`
	FupDisabledByFup bool   `json:"fupDisabledByFup" gorm:"column:fup_disabled_by_fup;default:false"`
	FupTriggerPeriod string `json:"fupTriggerPeriod" gorm:"column:fup_trigger_period;default:''"`

	DailyNotified   bool `json:"dailyNotified" gorm:"column:daily_notified;default:false"`
	WeeklyNotified  bool `json:"weeklyNotified" gorm:"column:weekly_notified;default:false"`
	MonthlyNotified bool `json:"monthlyNotified" gorm:"column:monthly_notified;default:false"`
}

func (ClientFupState) TableName() string { return "client_fup_states" }

// FUP action constants stored in ClientRecord.FupAction.
const (
	FupActionNotify            = "notify"
	FupActionDisableHours      = "disable_hours"
	FupActionDisableUntilReset = "disable_until_reset"
)

// FUP period identifiers for trigger tracking and notifications.
const (
	FupPeriodDaily   = "daily"
	FupPeriodWeekly  = "weekly"
	FupPeriodMonthly = "monthly"
)
