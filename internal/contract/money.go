package contract

import (
	"errors"
	"strconv"
	"strings"
)

// 面板对外的金额单位是**元**，不是分：站点设置、充值下单在接口和界面上都是
// 「12.34」这样的十进制字符串。钱包、账本和支付通道内部仍按整数分记账 ——
// 换算集中在这一对函数里，浮点误差就没有机会溜进账务。
//
// 字符串而不是浮点数还带来一个好处：JSON 往返不会掉精度，浏览器也不会把
// 0.1+0.2 那套带到金额上。
const (
	// MaxAmountCents 是一次充值金额的上限（一亿元）。它同时挡住溢出和
	// 手滑多打几个零的输入。
	MaxAmountCents = 1000000000000
	// 整数部分最多这些位，与 MaxAmountCents 对齐。
	maxAmountDigits = 13
)

var ErrAmount = errors.New("金额必须是元，最多两位小数")

// ParseAmount 把「元」的十进制写法解析成整数分。
//
// 只接受 ASCII 数字、可选的小数点与最多两位小数：不接受正负号、指数、
// 千分位和本地化写法 —— 这些写法各有各的歧义，让它们在入口处失败，
// 好过在账本里变成一个意想不到的数字。
func ParseAmount(v string) (int64, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, ErrAmount
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && len(frac) > 2 {
		return 0, ErrAmount
	}
	if whole == "" || len(whole) > maxAmountDigits {
		return 0, ErrAmount
	}
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, ErrAmount
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, ErrAmount
		}
	}
	// 「1.」与「1.5」都算合法输入，补零后再一起算。
	frac += strings.Repeat("0", 2-len(frac))
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	if units > MaxAmountCents/100 {
		return 0, ErrAmount
	}
	total := units*100 + cents
	if total > MaxAmountCents {
		return 0, ErrAmount
	}
	return total, nil
}

// FormatAmount 把整数分写回「元」，固定两位小数。站点设置与订单展示都用它，
// 所以「1 元」在界面上始终是 1.00，不会一会儿写 1 一会儿写 1.0。
func FormatAmount(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := strconv.FormatInt(cents/100, 10) + "." + strconv.FormatInt(cents%100+100, 10)[1:]
	if neg {
		return "-" + s
	}
	return s
}
