package thirdpartypromptaudit

import "context"

// similarSimhashDistance 是判定指令块相似的最大汉明距离。
const similarSimhashDistance = 3

// SimilarBlock 是相似指令块：候选范围包含熔断记录与已信任的指令块。
type SimilarBlock struct {
	Hash          string `json:"hash"`
	Preview       string `json:"preview"`
	Bytes         int    `json:"bytes"`
	Distance      int    `json:"distance"`
	BreakerStatus string `json:"breaker_status"`
	Trusted       bool   `json:"trusted"`
	DistinctUsers int    `json:"distinct_users"`
}

// SimilarBlocks 返回与指定指令块 simhash 距离不超过阈值的指令块，供逐块查看差异。
func (r *Repository) SimilarBlocks(ctx context.Context, hash string) ([]SimilarBlock, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sub2api_enhance.prompt_texts WHERE hash=$1 AND simhash IS NOT NULL)`, hash).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return []SimilarBlock{}, nil
	}
	rows, err := r.db.QueryContext(ctx, `SELECT p2.hash,left(p2.text,500),p2.bytes,
 COALESCE(bit_count((p2.simhash # p.simhash)::bit(64)),0) AS distance,
 COALESCE(b2.status,''),(pt.hash IS NOT NULL),COALESCE(b2.distinct_users,0)
 FROM sub2api_enhance.prompt_texts p
 JOIN sub2api_enhance.prompt_texts p2 ON p2.hash<>p.hash AND p2.kind='instruction' AND p2.simhash IS NOT NULL
 LEFT JOIN sub2api_enhance.instruction_breakers b2 ON b2.hash=p2.hash
 LEFT JOIN sub2api_enhance.prompt_trust pt ON pt.hash=p2.hash
 WHERE p.hash=$1 AND p.simhash IS NOT NULL
   AND (b2.hash IS NOT NULL OR pt.hash IS NOT NULL)
   AND bit_count((p2.simhash # p.simhash)::bit(64))<=$2
 ORDER BY distance,p2.hash`, hash, similarSimhashDistance)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]SimilarBlock, 0)
	for rows.Next() {
		var item SimilarBlock
		if err := rows.Scan(&item.Hash, &item.Preview, &item.Bytes, &item.Distance, &item.BreakerStatus, &item.Trusted, &item.DistinctUsers); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
