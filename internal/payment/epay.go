package payment

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type EPay struct{ Gateway, PID, Key, NotifyURL, ReturnURL string }

func EPaySign(v url.Values, key string) string {
	keys := []string{}
	for k := range v {
		if k != "sign" && k != "sign_type" && v.Get(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		pairs = append(pairs, k+"="+v.Get(k))
	}
	sum := md5.Sum([]byte(strings.Join(pairs, "&") + key))
	return hex.EncodeToString(sum[:])
}
func (e EPay) Create(order string, cents int64) (string, error) {
	u, err := url.Parse(e.Gateway)
	if err != nil || u.Scheme != "https" || u.Host == "" || e.PID == "" || e.Key == "" || cents <= 0 {
		return "", errors.New("EPay configuration invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/submit.php"
	v := url.Values{"pid": {e.PID}, "type": {"alipay"}, "out_trade_no": {order}, "notify_url": {e.NotifyURL}, "return_url": {e.ReturnURL}, "name": {"Wallet recharge"}, "money": {fmt.Sprintf("%d.%02d", cents/100, cents%100)}, "sign_type": {"MD5"}}
	v.Set("sign", EPaySign(v, e.Key))
	u.RawQuery = v.Encode()
	return u.String(), nil
}
func (e EPay) Verify(v url.Values) (order, transaction string, cents int64, err error) {
	for _, vs := range v {
		if len(vs) != 1 {
			return "", "", 0, errors.New("duplicate parameter")
		}
	}
	want := EPaySign(v, e.Key)
	if e.Key == "" || v.Get("pid") != e.PID || v.Get("sign_type") != "MD5" || subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(v.Get("sign")))) != 1 || v.Get("trade_status") != "TRADE_SUCCESS" {
		return "", "", 0, errors.New("invalid callback")
	}
	money := v.Get("money")
	parts := strings.Split(money, ".")
	if len(parts) != 2 || len(parts[1]) != 2 {
		return "", "", 0, errors.New("invalid money")
	}
	var whole, frac int64
	for _, c := range parts[0] + parts[1] {
		if c < '0' || c > '9' {
			return "", "", 0, errors.New("invalid money")
		}
	}
	if len(parts[0]) > 10 {
		return "", "", 0, errors.New("amount too large")
	}
	if _, err = fmt.Sscanf(money, "%d.%d", &whole, &frac); err != nil {
		return
	}
	cents = whole*100 + frac
	order = v.Get("out_trade_no")
	transaction = v.Get("trade_no")
	if order == "" || transaction == "" || cents <= 0 {
		return "", "", 0, errors.New("invalid payment")
	}
	return
}
