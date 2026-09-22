package commerce

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

// Channel 是运营方配置的一条收款通道。
type Channel struct {
	Adapter   payment.Adapter
	NotifyURL string
	ReturnURL string
	Method    string
	// FeeBPS 是这条通道额外收取的手续费，单位万分之一（150 就是 1.5%）；
	// FeeFixed 是固定部分，单位分。手续费加在充值金额**之上**：用户钱包到账
	// amount，实际付给网关 payable = amount + fee。这样「充值 100 元」这句话
	// 对用户和对账本始终是同一个意思。
	FeeBPS   int
	FeeFixed int64
	// Rate 是「1 单位加密货币值多少人民币」，按通道分别设置 —— 同一个面板可以
	// 接多条收款通道，各自的商户汇率未必相同。CryptoCurrency 为空或 Rate 不是
	// 正数时不产生加密货币报价。
	Rate           string
	CryptoCurrency string
}

var ErrPaymentUncertain = errors.New("payment creation uncertain; reconcile this order before retrying")

// Quote 算出一笔充值实际要付多少。比例部分按万分之一整数计算并向下取整到分 ——
// 少收半分钱的手续费，好过为了凑整引入浮点误差。
func (c Channel) Quote(amount int64) (payable, fee int64) {
	fee = amount*int64(c.FeeBPS)/10000 + c.FeeFixed
	if fee < 0 {
		fee = 0
	}
	return amount + fee, fee
}

// CryptoQuote 按通道汇率把实付人民币折算成加密货币金额，返回两位小数的十进制
// 字符串。向上取整到分：报价宁可多一分，也不能让用户少付。
//
// 这只是**报价**：真正收款金额由网关按它自己的商户汇率换算（Cryptomus 与
// BEpusdt 在下单时收人民币加目标币种，TokenPay 的 ActualAmount 以
// BaseCurrency 计价）。所以运营方要把同一个汇率同时配在通道和网关上，
// 两边才会一致。
func (c Channel) CryptoQuote(payable int64) string {
	if c.CryptoCurrency == "" || payable <= 0 {
		return ""
	}
	rate, ok := new(big.Rat).SetString(c.Rate)
	if !ok || rate.Sign() <= 0 {
		return ""
	}
	// 人民币分 → 元 → 按汇率折成币种金额 → 向上取整到分。
	crypto := new(big.Rat).Quo(new(big.Rat).SetFrac64(payable, 100), rate)
	scaled := new(big.Rat).Mul(crypto, big.NewRat(100, 1))
	units, remainder := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if remainder.Sign() > 0 {
		units.Add(units, big.NewInt(1))
	}
	if !units.IsInt64() {
		return ""
	}
	return contract.FormatAmount(units.Int64())
}

// resolvePayable 补齐展示字段：手续费是实付减去到账，加密报价按通道汇率算。
// 老订单没有 payable_cents 的值（0），按「实付等于到账」处理。
func (o *Order) resolvePayable(configured Channel) {
	if o.Payable <= 0 {
		o.Payable = o.Amount
	}
	o.Fee = o.Payable - o.Amount
	o.CryptoCurrency = configured.CryptoCurrency
	o.Rate = configured.Rate
	o.PayableCrypto = configured.CryptoQuote(o.Payable)
}

// CreateAdapterOrder durably allocates an order before calling any gateway.
// An ambiguous creation is never retried as a new external order automatically.
func (s *Service) CreateAdapterOrder(ctx context.Context, user, channel, key string, amount int64, configured Channel, clientIP string) (Order, error) {
	if s.PaymentAllowed != nil {
		if err := s.PaymentAllowed(ctx, amount); err != nil {
			return Order{}, err
		}
	}
	var order Order
	if configured.Adapter == nil || amount <= 0 || amount > contract.MaxAmountCents || key == "" || len(key) > 128 {
		return order, errors.New("invalid or unavailable channel/amount/key")
	}
	payable, fee := configured.Quote(amount)
	fresh := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, e := s.walletTx(ctx, tx, user); e != nil {
			return e
		}
		var created string
		e := tx.QueryRowContext(ctx, s.q("SELECT id,channel,amount,payable_cents,status,payment_url,created_at FROM commerce_orders WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&order.ID, &order.Channel, &order.Amount, &order.Payable, &order.Status, &order.PaymentURL, &created)
		if e == nil {
			if order.Channel != channel || order.Amount != amount {
				return ErrConflict
			}
			order.Currency = contract.SettlementCurrency
			order.CreatedAt = parse(created)
			order.resolvePayable(configured)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		order = Order{ID: id(), Channel: channel, Amount: amount, Payable: payable, Fee: fee, Currency: contract.SettlementCurrency, Status: "pending", CreatedAt: s.Now().UTC()}
		if _, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_orders(id,user_id,channel,amount,payable_cents,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,'pending','',?,?)"), order.ID, user, channel, amount, payable, stamp(order.CreatedAt), key); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_attempts(order_id,state,provider_id,updated_at) VALUES(?,'creating','',?)"), order.ID, stamp(s.Now()))
		if e == nil {
			e = s.scheduleReconciliation(ctx, tx, order.ID)
		}
		fresh = e == nil
		return e
	})
	if err != nil {
		return order, err
	}
	order.resolvePayable(configured)
	if !fresh {
		return order, nil
	}
	created, callErr := configured.Adapter.Create(ctx, payment.CreateRequest{OrderID: order.ID, UserKey: user, Name: "钱包充值", Currency: contract.SettlementCurrency, AmountCents: payable, NotifyURL: configured.NotifyURL, ReturnURL: configured.ReturnURL, ClientIP: clientIP, Method: configured.Method})
	if callErr == nil && (created.OrderID != order.ID || created.Currency != contract.SettlementCurrency || created.AmountCents != payable || created.PaymentURL == "") {
		callErr = payment.ErrProtocol
	}
	// Persist uncertainty even when the HTTP client disconnected during creation.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = s.Write(persistCtx, func(tx *sql.Tx) error {
		state := "ready"
		provider := ""
		if callErr != nil {
			state = "uncertain"
		} else {
			provider = created.TransactionID
		}
		if _, e := tx.ExecContext(persistCtx, s.q("UPDATE commerce_attempts SET state=?,provider_id=?,updated_at=? WHERE order_id=?"), state, provider, stamp(s.Now()), order.ID); e != nil {
			return e
		}
		if callErr == nil {
			_, e := tx.ExecContext(persistCtx, s.q("UPDATE commerce_orders SET payment_url=? WHERE id=?"), created.PaymentURL, order.ID)
			return e
		}
		return nil
	})
	if err != nil {
		return order, err
	}
	if callErr != nil {
		return order, ErrPaymentUncertain
	}
	order.PaymentURL = created.PaymentURL
	return order, nil
}

// ReconcileOrder uses the channel's authenticated query and the same financial
// confirmation path as callbacks. It never infers payment from a return URL.
func (s *Service) ReconcileOrder(ctx context.Context, user, orderID string, channels map[string]Channel) (Order, error) {
	var order Order
	var provider, created string
	err := s.DB.QueryRowContext(ctx, s.q("SELECT o.id,o.channel,o.amount,o.payable_cents,o.status,o.payment_url,o.created_at,COALESCE(a.provider_id,'') FROM commerce_orders o LEFT JOIN commerce_attempts a ON a.order_id=o.id WHERE o.id=? AND o.user_id=?"), orderID, user).Scan(&order.ID, &order.Channel, &order.Amount, &order.Payable, &order.Status, &order.PaymentURL, &created, &provider)
	if err != nil {
		return order, err
	}
	order.Currency = contract.SettlementCurrency
	order.CreatedAt = parse(created)
	c, ok := channels[order.Channel]
	order.resolvePayable(c)
	if order.Status == "paid" || order.Status == "paid_late" || order.Status == "refunded" || order.Status == "partially_refunded" {
		return order, nil
	}
	if !ok || c.Adapter == nil || !c.Adapter.Capabilities().Query {
		return order, payment.ErrUnsupported
	}
	queryCtx, cancel := context.WithTimeout(ctx, reconciliationTimeout)
	defer cancel()
	status, err := c.Adapter.Query(queryCtx, payment.QueryRequest{OrderID: orderID, TransactionID: provider})
	if err != nil {
		return order, err
	}
	if err = payment.ValidateExpected(status, order.ID, order.Payable, contract.SettlementCurrency); err != nil {
		return order, err
	}
	if status.State == payment.Paid {
		if err = s.ConfirmStatus(ctx, order.Channel, status); err != nil {
			return order, err
		}
		if err = s.DB.QueryRowContext(ctx, s.q("SELECT status FROM commerce_orders WHERE id=?"), orderID).Scan(&order.Status); err != nil {
			return order, err
		}
	}
	// Pending/expired gateway states never discard a subsequently valid receipt.
	return order, nil
}

// ConfirmStatus 用网关报回来的实付金额核对订单，再按**到账金额**入账。
//
// 两者在有手续费的通道上不相等：网关收的是实付，钱包记的是充值额。核对用前者
// —— 那才是真的付了多少钱；入账用后者 —— 那是订单承诺给用户的东西。
func (s *Service) ConfirmStatus(ctx context.Context, channel string, status payment.Status) error {
	if status.State != payment.Paid {
		return payment.ErrProtocol
	}
	// 先核对网关报回来的订单号、金额、币种与交易号，再进事务入账。老订单的
	// payable_cents 是 0，按到账金额比对。
	var expected int64
	if err := s.DB.QueryRowContext(ctx, s.q("SELECT CASE WHEN payable_cents>0 THEN payable_cents ELSE amount END FROM commerce_orders WHERE id=? AND channel=?"), status.OrderID, channel).Scan(&expected); err != nil {
		return err
	}
	if err := payment.ValidateExpected(status, status.OrderID, expected, contract.SettlementCurrency); err != nil {
		return err
	}
	return s.ConfirmPayment(ctx, channel, status.OrderID, status.TransactionID, status.AmountCents)
}
