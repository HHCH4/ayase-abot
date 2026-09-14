package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var ErrInvalidTokenCalibration = errors.New("模型 token 校准数据无效")

// RecordContextUsageCalibration folds one correlated manifest/usage sample
// into the model's bounded tokenizer metadata. The profile is updated only
// from non-secret counters; no prompt or provider response is retained.
//
// A single sample never changes the estimator. Once the explicit minimum
// sample count is reached, only an underestimate can increase the multiplier,
// and the multiplier is capped at MaxTokenizerSafetyMultiplier. This keeps
// the next request conservative without allowing noisy usage to shrink the
// available context or claim tokenizer exactness.
func (r *Registry) RecordContextUsageCalibration(ctx context.Context, providerID, modelID string, estimated, actual int) error {
	if r == nil {
		return fmt.Errorf("%w: Registry 为空", ErrInvalidTokenCalibration)
	}
	if estimated < 0 || actual < 0 {
		return fmt.Errorf("%w: token 数不能为负数", ErrInvalidTokenCalibration)
	}
	providerID = strings.TrimSpace(providerID)
	modelID = strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return fmt.Errorf("%w: provider/model 不能为空", ErrInvalidTokenCalibration)
	}

	// Usage backfill may arrive concurrently for multiple model calls. Serialize
	// the read/merge/write sequence so one sample cannot silently erase another.
	r.calibrationMu.Lock()
	defer r.calibrationMu.Unlock()

	item, err := r.Get(providerID)
	if err != nil {
		return err
	}
	index := -1
	for i := range item.Models {
		if item.Models[i].ID == modelID {
			index = i
			break
		}
	}
	if index < 0 {
		return ErrNoModel
	}
	model := item.Models[index]
	profile := model.Capabilities
	if profile == nil {
		value := DefaultCapabilities(item, model)
		profile = &value
	}
	updated := *profile
	tokenizer := EffectiveTokenizer(&updated)
	calibration := tokenizer.Calibration
	if calibration.SafetyMultiplier < 1 {
		calibration.SafetyMultiplier = 1
	}
	if calibration.Samples < MaxTokenizerCalibrationSamples {
		calibration.Samples++
	}
	calibration.EstimatedTokens = saturatingAddInt(calibration.EstimatedTokens, estimated)
	calibration.ActualTokens = saturatingAddInt(calibration.ActualTokens, actual)
	errorTokens := int64(actual) - int64(estimated)
	calibration.ErrorTokens = saturatingAddInt64(calibration.ErrorTokens, errorTokens)
	if errorTokens < 0 {
		calibration.AbsoluteErrorTokens = saturatingAddInt64(calibration.AbsoluteErrorTokens, -errorTokens)
	} else {
		calibration.AbsoluteErrorTokens = saturatingAddInt64(calibration.AbsoluteErrorTokens, errorTokens)
	}
	calibration.Source = "runtime_usage"
	calibration.UpdatedAt = time.Now().UTC()
	if calibration.Samples >= MinTokenizerCalibrationSamples && actual > estimated {
		ratio := conservativeTokenRatio(estimated, actual)
		if ratio > calibration.SafetyMultiplier {
			calibration.SafetyMultiplier = ratio
		}
	}
	if calibration.SafetyMultiplier > MaxTokenizerSafetyMultiplier {
		calibration.SafetyMultiplier = MaxTokenizerSafetyMultiplier
	}
	tokenizer.Calibration = calibration
	if calibration.Samples >= MinTokenizerCalibrationSamples && calibration.SafetyMultiplier > 1 && tokenizer.Quality == DefaultTokenizerQuality {
		tokenizer.Quality = "calibrated"
	}
	updated.Tokenizer = tokenizer
	updated.UpdatedAt = calibration.UpdatedAt
	baseRevision := updated.SourceRevision
	if baseRevision == "" {
		baseRevision = "catalog"
	}
	if marker := strings.Index(baseRevision, ":usage-calibration:"); marker >= 0 {
		baseRevision = baseRevision[:marker]
	}
	updated.SourceRevision = baseRevision + ":usage-calibration:" + calibration.UpdatedAt.Format("20060102T150405.000000000Z")
	if err := updated.Validate(); err != nil {
		return err
	}
	model.Capabilities = &updated
	item.Models[index] = model
	if _, err := r.Save(ctx, SaveRequest{Provider: item}); err != nil {
		return fmt.Errorf("保存 token 校准失败: %w", err)
	}
	return nil
}

func conservativeTokenRatio(estimated, actual int) float64 {
	if actual <= 0 {
		return 1
	}
	if estimated <= 0 {
		return MaxTokenizerSafetyMultiplier
	}
	ratio := float64(actual) / float64(estimated)
	// A small fixed contingency protects the next request from rounding and
	// wrapper overhead while the hard cap prevents one bad sample from
	// consuming the entire context window.
	ratio *= 1.10
	if ratio < 1 {
		return 1
	}
	return math.Min(MaxTokenizerSafetyMultiplier, ratio)
}

func saturatingAddInt(left, right int) int {
	if right <= 0 {
		return left
	}
	if left > int(^uint(0)>>1)-right {
		return int(^uint(0) >> 1)
	}
	return left + right
}

func saturatingAddInt64(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}
