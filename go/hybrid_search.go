// hybrid_search.go — Гибридный поиск v3.7: BM25 + косинусное сходство.
//
// Алгоритм:
//  1. BM25 — лексический поиск по точным терминам (хорошо для конкретных слов/имён)
//  2. Cosine similarity — семантический поиск по эмбеддингам (хорошо для смысла)
//  3. Линейная комбинация: score = α·cosine + (1-α)·bm25_norm
//
// BM25 формула:
//
//	score(d,q) = Σ IDF(qi) * tf(qi,d)*(k1+1) / (tf(qi,d) + k1*(1-b+b*|d|/avgdl))
//	k1=1.5, b=0.75 — стандартные параметры Okapi BM25
package main

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
)

// ── Параметры BM25 ────────────────────────────────────────────────────────

const (
	bm25K1 = 1.5  // насыщение частоты термина
	bm25B  = 0.75 // нормализация длины документа
)

// hybridAlphaDefault — доля семантического поиска в финальном скоре.
// 0.0 = только BM25, 1.0 = только cosine, 0.6 = больше семантики.
const hybridAlphaDefault = 0.6

// ── BM25 Индекс ───────────────────────────────────────────────────────────

// bm25Doc — статистика одного документа-чанка для BM25.
type bm25Doc struct {
	chunkID string
	termFreq map[string]float64 // tf: количество вхождений термина
	length   int                // количество токенов
}

// BM25Index — инвертированный индекс для BM25-поиска по чанкам.
type BM25Index struct {
	mu       sync.RWMutex
	docs     []bm25Doc          // все чанки
	df       map[string]int     // document frequency: сколько чанков содержат термин
	avgDocLen float64           // средняя длина документа (в токенах)
	N        int                // общее число чанков
}

// NewBM25Index создаёт пустой BM25-индекс.
func NewBM25Index() *BM25Index {
	return &BM25Index{
		df: make(map[string]int),
	}
}

// Build перестраивает индекс из списка чанков.
// Вызывается после AddDocument и DeleteDoc.
func (idx *BM25Index) Build(chunks []Chunk) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.docs = make([]bm25Doc, 0, len(chunks))
	idx.df = make(map[string]int)
	totalLen := 0

	for _, c := range chunks {
		tokens := bm25Tokenize(c.Text)
		tf := make(map[string]float64, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		idx.docs = append(idx.docs, bm25Doc{
			chunkID:  c.ID,
			termFreq: tf,
			length:   len(tokens),
		})
		totalLen += len(tokens)
		// document frequency: считаем уникальные термины по чанкам
		for t := range tf {
			idx.df[t]++
		}
	}

	idx.N = len(chunks)
	if idx.N > 0 {
		idx.avgDocLen = float64(totalLen) / float64(idx.N)
	} else {
		idx.avgDocLen = 1
	}
}

// Score возвращает BM25-скоры для каждого чанка по запросу.
// Результат: map chunkID → score.
func (idx *BM25Index) Score(query string) map[string]float64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	scores := make(map[string]float64, len(idx.docs))
	queryTerms := bm25Tokenize(query)
	if len(queryTerms) == 0 || idx.N == 0 {
		return scores
	}

	for _, doc := range idx.docs {
		s := 0.0
		for _, term := range queryTerms {
			tf := doc.termFreq[term]
			if tf == 0 {
				continue
			}
			df := float64(idx.df[term])
			if df == 0 {
				continue
			}
			// IDF (Robertson & Sparck Jones)
			idf := math.Log((float64(idx.N)-df+0.5)/(df+0.5) + 1)
			// TF нормализованный по длине документа
			norm := tf * (bm25K1 + 1) /
				(tf + bm25K1*(1-bm25B+bm25B*float64(doc.length)/idx.avgDocLen))
			s += idf * norm
		}
		if s > 0 {
			scores[doc.chunkID] = s
		}
	}
	return scores
}

// ── Гибридный поиск ───────────────────────────────────────────────────────

// HybridSearcher — комбинирует BM25 и векторный поиск.
type HybridSearcher struct {
	rag   *RAG
	bm25  *BM25Index
}

// NewHybridSearcher создаёт гибридный поисковик.
func NewHybridSearcher(rag *RAG) *HybridSearcher {
	hs := &HybridSearcher{
		rag:  rag,
		bm25: NewBM25Index(),
	}
	hs.RebuildBM25() // строим индекс из уже загруженных чанков
	return hs
}

// RebuildBM25 перестраивает BM25-индекс из текущих чанков RAG.
// Вызывать после каждого AddDocument / DeleteDoc.
func (hs *HybridSearcher) RebuildBM25() {
	chunks := hs.rag.ListChunks()
	hs.bm25.Build(chunks)
}

// Search выполняет гибридный поиск: BM25 + cosine similarity.
//
//   - query — текст запроса
//   - topK  — сколько результатов вернуть
//   - alpha — вес семантического поиска [0.0, 1.0]; -1 = использовать дефолт
func (hs *HybridSearcher) Search(ctx context.Context, query string, topK int, alpha float64) ([]SearchResult, error) {
	if alpha < 0 {
		alpha = hybridAlphaDefault
	}

	// ── Семантический поиск (cosine) ─────────────────────────────────
	// Запрашиваем больше кандидатов для последующего ре-ранжирования
	candidateK := topK * 3
	if candidateK < 20 {
		candidateK = 20
	}
	cosineResults, err := hs.rag.Search(ctx, query, candidateK)
	if err != nil {
		return nil, err
	}
	if len(cosineResults) == 0 {
		return nil, nil
	}

	// Строим map chunkID → cosine score для быстрого доступа
	cosineScores := make(map[string]float64, len(cosineResults))
	maxCosine := 0.0
	for _, r := range cosineResults {
		cosineScores[r.Chunk.ID] = r.Similarity
		if r.Similarity > maxCosine {
			maxCosine = r.Similarity
		}
	}

	// ── BM25-поиск ───────────────────────────────────────────────────
	bm25Scores := hs.bm25.Score(query)
	maxBM25 := 0.0
	for _, s := range bm25Scores {
		if s > maxBM25 {
			maxBM25 = s
		}
	}

	// ── Нормализация и объединение ───────────────────────────────────
	// Нормализуем оба скора в [0, 1], затем комбинируем линейно.
	type scored struct {
		result SearchResult
		hybrid float64
	}

	// Объединяем все кандидаты из обоих поисков
	seen := make(map[string]bool)
	var candidates []scored

	for _, r := range cosineResults {
		id := r.Chunk.ID
		if seen[id] {
			continue
		}
		seen[id] = true

		normCosine := 0.0
		if maxCosine > 0 {
			normCosine = r.Similarity / maxCosine
		}
		normBM25 := 0.0
		if maxBM25 > 0 {
			normBM25 = bm25Scores[id] / maxBM25
		}

		hybrid := alpha*normCosine + (1-alpha)*normBM25
		candidates = append(candidates, scored{r, hybrid})
	}

	// Добавляем чанки, найденные BM25, но не вошедшие в cosine top-K
	allChunks := hs.rag.ListChunks()
	chunkMap := make(map[string]Chunk, len(allChunks))
	for _, c := range allChunks {
		chunkMap[c.ID] = c
	}

	for id, bScore := range bm25Scores {
		if seen[id] {
			continue
		}
		seen[id] = true
		chunk, ok := chunkMap[id]
		if !ok {
			continue
		}
		normBM25 := 0.0
		if maxBM25 > 0 {
			normBM25 = bScore / maxBM25
		}
		hybrid := alpha*0 + (1-alpha)*normBM25
		candidates = append(candidates, scored{
			result: SearchResult{Chunk: chunk, Similarity: 0},
			hybrid: hybrid,
		})
	}

	// Сортируем по убыванию гибридного скора
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].hybrid > candidates[j].hybrid
	})

	// Берём topK лучших с порогом качества
	if topK > len(candidates) {
		topK = len(candidates)
	}
	out := make([]SearchResult, 0, topK)
	for i := 0; i < topK; i++ {
		c := candidates[i]
		// Используем гибридный скор как итоговую схожесть
		c.result.Similarity = c.hybrid
		// Отсекаем слишком нерелевантные результаты
		if c.hybrid < 0.15 {
			break
		}
		out = append(out, c.result)
	}
	return out, nil
}

// ── Токенизатор для BM25 ──────────────────────────────────────────────────

// bm25Tokenize разбивает текст на токены для BM25.
// Нижний регистр, только слова длиной ≥ 2, без стоп-слов.
func bm25Tokenize(text string) []string {
	lower := strings.ToLower(text)
	raw := strings.FieldsFunc(lower, func(r rune) bool {
		// разделяем по не-буквам и не-цифрам
		return r < 'a' && (r < '0' || r > '9') && r != '-' ||
			r > 'z' && r < 'а' ||
			r > 'я' && r != 'ё'
	})

	tokens := raw[:0]
	for _, w := range raw {
		if len([]rune(w)) >= 2 && !bm25StopWord(w) {
			tokens = append(tokens, w)
		}
	}
	return tokens
}

// bm25StopWord возвращает true для стоп-слов (игнорируются при индексации).
// Стоп-слова не несут смысла и засоряют индекс.
func bm25StopWord(w string) bool {
	// Русские и английские стоп-слова объединены в один case.
	switch w {
	case
		// Русские
		"и", "в", "во", "не", "что", "он", "на", "я", "с", "со",
		"как", "а", "то", "все", "она", "так", "его", "но", "да",
		"ты", "к", "у", "же", "вы", "за", "бы", "по", "только",
		"её", "мне", "было", "вот", "от", "меня", "ещё", "нет",
		"о", "из", "ему", "теперь", "когда", "даже", "ну", "вдруг",
		"ли", "если", "уже", "или", "ни", "быть", "был", "него",
		"до", "вас", "нибудь", "опять", "уж", "вам", "ведь", "там",
		"потом", "себя", "ничего", "ей", "может", "они", "тут",
		"где", "есть", "надо", "ней", "для", "мы", "тебя", "их",
		"чем", "была", "сам", "чтоб", "без", "будто", "чего", "раз",
		"тоже", "себе", "под", "будет", "ж", "тогда", "кто", "этот",
		"того", "потому", "этого", "какой", "этой", "между",
		// Английские
		"the", "a", "an", "and", "or", "but", "in", "on", "at",
		"to", "for", "of", "with", "by", "from", "is", "are",
		"was", "were", "be", "been", "being", "have", "has", "had",
		"do", "does", "did", "will", "would", "could", "should",
		"may", "might", "shall", "can", "this", "that", "these",
		"those", "it", "its", "not", "no", "so", "if", "as", "up":
		return true
	}
	return false
}
