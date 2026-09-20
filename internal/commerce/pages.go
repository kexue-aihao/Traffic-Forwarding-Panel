package commerce

import "context"

func (s *Service) PlansPage(ctx context.Context, page, size int) ([]Plan, int, error) {
	var total int
	if e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM commerce_plans").Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT id,name,price,quota,months FROM commerce_plans ORDER BY id LIMIT ? OFFSET ?"), size, (page-1)*size)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []Plan{}
	for rows.Next() {
		var p Plan
		if e = rows.Scan(&p.ID, &p.Name, &p.Price, &p.Quota, &p.Months); e != nil {
			return nil, 0, e
		}
		items = append(items, p)
	}
	return items, total, rows.Err()
}
func (s *Service) OrdersPage(ctx context.Context, user string, page, size int) ([]Order, int, error) {
	var total int
	if e := s.DB.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM commerce_orders WHERE user_id=?"), user).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT id,channel,amount,status,payment_url,created_at FROM commerce_orders WHERE user_id=? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?"), user, size, (page-1)*size)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []Order{}
	for rows.Next() {
		var o Order
		var date string
		if e = rows.Scan(&o.ID, &o.Channel, &o.Amount, &o.Status, &o.PaymentURL, &date); e != nil {
			return nil, 0, e
		}
		o.Currency = "CNY"
		o.CreatedAt = parse(date)
		items = append(items, o)
	}
	return items, total, rows.Err()
}
func (s *Service) LedgerPage(ctx context.Context, user string, page, size int) ([]Ledger, int, error) {
	var total int
	if e := s.DB.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM commerce_ledger WHERE user_id=?"), user).Scan(&total); e != nil {
		return nil, 0, e
	}
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT id,amount,balance,kind,reference_id,created_at FROM commerce_ledger WHERE user_id=? ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?"), user, size, (page-1)*size)
	if e != nil {
		return nil, 0, e
	}
	defer rows.Close()
	items := []Ledger{}
	for rows.Next() {
		var l Ledger
		var date string
		if e = rows.Scan(&l.ID, &l.Amount, &l.Balance, &l.Kind, &l.Reference, &date); e != nil {
			return nil, 0, e
		}
		l.CreatedAt = parse(date)
		items = append(items, l)
	}
	return items, total, rows.Err()
}
