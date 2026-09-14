// Package basic provides small, deterministic tools that are useful in every
// conversation and do not require a workspace or an external service.
package basic

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

const maxExpressionLength = 512

type timeArgs struct {
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA 时区名称，例如 Asia/Shanghai、Europe/London；省略时使用 UTC"`
}

type timeResult struct {
	Timezone      string `json:"timezone"`
	LocalDatetime string `json:"local_datetime"`
	UTCOffset     string `json:"utc_offset"`
	Weekday       string `json:"weekday"`
	Unix          int64  `json:"unix"`
}

type calculateArgs struct {
	Expression string `json:"expression" jsonschema:"只包含数字、括号、常量 pi/e、运算符 + - * / % ^，以及 sqrt/abs/round/floor/ceil/min/max/pow 的算式"`
}

type calculateResult struct {
	Expression string  `json:"expression"`
	Result     float64 `json:"result"`
	Formatted  string  `json:"formatted"`
}

// Tools creates the built-in tools exposed to every Agent invocation.
func Tools() ([]tool.Tool, error) {
	return toolsWithClock(time.Now)
}

func toolsWithClock(now func() time.Time) ([]tool.Tool, error) {
	currentTime, err := functiontool.New(functiontool.Config{
		Name:        "current_time",
		Description: "查询指定 IANA 时区的准确当前日期、时间、星期和 UTC 偏移。用户询问当前时间或日期时使用，不要凭模型知识猜测。",
	}, func(_ adkagent.Context, args timeArgs) (timeResult, error) {
		zone := strings.TrimSpace(args.Timezone)
		if zone == "" {
			zone = "UTC"
		}
		location, loadErr := time.LoadLocation(zone)
		if loadErr != nil {
			return timeResult{}, fmt.Errorf("无效的 IANA 时区 %q: %w", zone, loadErr)
		}
		value := now().In(location)
		_, offsetSeconds := value.Zone()
		return timeResult{
			Timezone: zone, LocalDatetime: value.Format(time.RFC3339),
			UTCOffset: formatUTCOffset(offsetSeconds), Weekday: value.Weekday().String(), Unix: value.Unix(),
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("创建当前时间工具失败: %w", err)
	}

	calculator, err := functiontool.New(functiontool.Config{
		Name:        "calculate",
		Description: "精确计算一个数学表达式。需要算术计算时使用，禁止传入代码、变量、单位或自然语言。",
	}, func(_ adkagent.Context, args calculateArgs) (calculateResult, error) {
		expression := strings.TrimSpace(args.Expression)
		value, calculateErr := evaluate(expression)
		if calculateErr != nil {
			return calculateResult{}, calculateErr
		}
		return calculateResult{
			Expression: expression,
			Result:     value,
			Formatted:  strconv.FormatFloat(value, 'g', -1, 64),
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("创建计算工具失败: %w", err)
	}
	return []tool.Tool{currentTime, calculator}, nil
}

func formatUTCOffset(seconds int) string {
	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}
	return fmt.Sprintf("%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
}

func evaluate(expression string) (float64, error) {
	if expression == "" {
		return 0, errors.New("表达式不能为空")
	}
	if len(expression) > maxExpressionLength {
		return 0, fmt.Errorf("表达式不能超过 %d 个字符", maxExpressionLength)
	}
	p := expressionParser{source: expression}
	value, err := p.parseExpression()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.position != len(p.source) {
		return 0, p.errorf("无法识别的内容")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("计算结果不是有限数值")
	}
	return value, nil
}

type expressionParser struct {
	source   string
	position int
}

func (p *expressionParser) parseExpression() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		if p.consume('+') {
			right, parseErr := p.parseTerm()
			if parseErr != nil {
				return 0, parseErr
			}
			left += right
		} else if p.consume('-') {
			right, parseErr := p.parseTerm()
			if parseErr != nil {
				return 0, parseErr
			}
			left -= right
		} else {
			return left, nil
		}
	}
}

func (p *expressionParser) parseTerm() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		if p.consume('*') {
			right, parseErr := p.parseUnary()
			if parseErr != nil {
				return 0, parseErr
			}
			left *= right
		} else if p.consume('/') {
			right, parseErr := p.parseUnary()
			if parseErr != nil {
				return 0, parseErr
			}
			if right == 0 {
				return 0, errors.New("不能除以零")
			}
			left /= right
		} else if p.consume('%') {
			right, parseErr := p.parseUnary()
			if parseErr != nil {
				return 0, parseErr
			}
			if right == 0 {
				return 0, errors.New("不能对零取余")
			}
			left = math.Mod(left, right)
		} else {
			return left, nil
		}
	}
}

func (p *expressionParser) parseUnary() (float64, error) {
	if p.consume('+') {
		return p.parseUnary()
	}
	if p.consume('-') {
		value, err := p.parseUnary()
		return -value, err
	}
	return p.parsePower()
}

func (p *expressionParser) parsePower() (float64, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return 0, err
	}
	if !p.consume('^') {
		return left, nil
	}
	right, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	return math.Pow(left, right), nil
}

func (p *expressionParser) parsePrimary() (float64, error) {
	p.skipSpaces()
	if p.consume('(') {
		value, err := p.parseExpression()
		if err != nil {
			return 0, err
		}
		if !p.consume(')') {
			return 0, p.errorf("缺少右括号")
		}
		return value, nil
	}
	if p.position >= len(p.source) {
		return 0, p.errorf("缺少数字或表达式")
	}
	if isIdentifierStart(rune(p.source[p.position])) {
		name := p.parseIdentifier()
		switch name {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		}
		if !p.consume('(') {
			return 0, p.errorf("未知常量 %q", name)
		}
		args, err := p.parseArguments()
		if err != nil {
			return 0, err
		}
		return applyFunction(name, args)
	}
	return p.parseNumber()
}

func (p *expressionParser) parseArguments() ([]float64, error) {
	if p.consume(')') {
		return nil, nil
	}
	var args []float64
	for {
		value, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		args = append(args, value)
		if p.consume(')') {
			return args, nil
		}
		if !p.consume(',') {
			return nil, p.errorf("函数参数之间需要逗号")
		}
	}
}

func (p *expressionParser) parseNumber() (float64, error) {
	p.skipSpaces()
	start := p.position
	hasDigit := false
	for p.position < len(p.source) && p.source[p.position] >= '0' && p.source[p.position] <= '9' {
		p.position++
		hasDigit = true
	}
	if p.position < len(p.source) && p.source[p.position] == '.' {
		p.position++
		for p.position < len(p.source) && p.source[p.position] >= '0' && p.source[p.position] <= '9' {
			p.position++
			hasDigit = true
		}
	}
	if !hasDigit {
		return 0, p.errorf("需要数字")
	}
	if p.position < len(p.source) && (p.source[p.position] == 'e' || p.source[p.position] == 'E') {
		p.position++
		if p.position < len(p.source) && (p.source[p.position] == '+' || p.source[p.position] == '-') {
			p.position++
		}
		exponentStart := p.position
		for p.position < len(p.source) && p.source[p.position] >= '0' && p.source[p.position] <= '9' {
			p.position++
		}
		if exponentStart == p.position {
			return 0, p.errorf("科学计数法指数无效")
		}
	}
	value, err := strconv.ParseFloat(p.source[start:p.position], 64)
	if err != nil {
		return 0, p.errorf("数字无效")
	}
	return value, nil
}

func applyFunction(name string, args []float64) (float64, error) {
	one := func(fn func(float64) float64) (float64, error) {
		if len(args) != 1 {
			return 0, fmt.Errorf("函数 %s 需要 1 个参数", name)
		}
		return fn(args[0]), nil
	}
	switch name {
	case "sqrt":
		if len(args) != 1 || args[0] < 0 {
			return 0, errors.New("sqrt 需要 1 个非负参数")
		}
		return math.Sqrt(args[0]), nil
	case "abs":
		return one(math.Abs)
	case "round":
		return one(math.Round)
	case "floor":
		return one(math.Floor)
	case "ceil":
		return one(math.Ceil)
	case "pow":
		if len(args) != 2 {
			return 0, errors.New("函数 pow 需要 2 个参数")
		}
		return math.Pow(args[0], args[1]), nil
	case "min", "max":
		if len(args) < 2 {
			return 0, fmt.Errorf("函数 %s 至少需要 2 个参数", name)
		}
		value := args[0]
		for _, item := range args[1:] {
			if name == "min" {
				value = math.Min(value, item)
			} else {
				value = math.Max(value, item)
			}
		}
		return value, nil
	default:
		return 0, fmt.Errorf("不支持函数 %q", name)
	}
}

func (p *expressionParser) consume(expected byte) bool {
	p.skipSpaces()
	if p.position >= len(p.source) || p.source[p.position] != expected {
		return false
	}
	p.position++
	return true
}

func (p *expressionParser) skipSpaces() {
	for p.position < len(p.source) && unicode.IsSpace(rune(p.source[p.position])) {
		p.position++
	}
}

func (p *expressionParser) parseIdentifier() string {
	start := p.position
	for p.position < len(p.source) {
		r := rune(p.source[p.position])
		if !unicode.IsLetter(r) && r != '_' {
			break
		}
		p.position++
	}
	return strings.ToLower(p.source[start:p.position])
}

func (p *expressionParser) errorf(format string, args ...any) error {
	return fmt.Errorf("表达式第 %d 个字符附近: %s", p.position+1, fmt.Sprintf(format, args...))
}

func isIdentifierStart(r rune) bool { return unicode.IsLetter(r) || r == '_' }
