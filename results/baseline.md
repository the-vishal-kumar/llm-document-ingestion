# Evaluation: rule-based baseline

Produced by `ingest eval -extractor rules -n 300 -seed 42 -threshold 0.85`. Synthetic documents only; see `internal/synth` for how they are generated.

Documents: 300 (chat 37, csv 49, eml 73, pdf 42, txt 62, xlsx 37).

## Routing at threshold 0.85

| Metric | Value |
|---|---|
| Straight-through rate (auto-accepted / all) | 67.7% (203/300) |
| Error rate among auto-accepted | 0.0% (0/203) |
| Documents fully correct, any route | 85.7% (257/300) |
| Sent to human review | 97 |

A document is "fully correct" when every header field, every line field and every catalog match equals ground truth.

## Field-level extraction

| Field | Support | Precision | Recall | F1 |
|---|---:|---:|---:|---:|
| doc_type | 300 | 1.000 | 1.000 | 1.000 |
| buyer | 300 | 1.000 | 0.877 | 0.934 |
| po_number | 300 | 1.000 | 1.000 | 1.000 |
| po_date | 249 | 1.000 | 1.000 | 1.000 |
| delivery_date | 253 | 1.000 | 0.874 | 0.932 |
| currency | 260 | 1.000 | 0.935 | 0.966 |
| total | 218 | 1.000 | 0.876 | 0.934 |
| part_number | 1015 | 0.996 | 0.984 | 0.990 |
| description | 1077 | 0.987 | 0.982 | 0.985 |
| quantity | 1077 | 0.985 | 0.981 | 0.983 |
| unit | 1077 | 0.999 | 0.994 | 0.997 |
| unit_price | 887 | 0.997 | 0.991 | 0.994 |
| catalog_sku | 1077 | 0.995 | 0.991 | 0.993 |

Line fields are aligned by position. Support counts ground-truth values that are present; a wrong value counts as one false positive and one false negative.

## By format

| Format | Docs | Straight-through | Errors among auto-accepted | Fully correct |
|---|---:|---:|---:|---:|
| chat | 37 | 0.0% | n/a | 0.0% |
| csv | 49 | 77.6% | 0.0% | 93.9% |
| eml | 73 | 72.6% | 0.0% | 98.6% |
| pdf | 42 | 83.3% | 0.0% | 100.0% |
| txt | 62 | 72.6% | 0.0% | 96.8% |
| xlsx | 37 | 86.5% | 0.0% | 100.0% |

## Threshold sweep

Re-thresholds the recorded document confidence. Use it to pick the operating point: lower thresholds buy straight-through rate with errors.

| Threshold | Straight-through | Errors among auto-accepted |
|---:|---:|---:|
| 0.50 | 79.0% | 2.1% (5) |
| 0.60 | 68.3% | 0.0% (0) |
| 0.70 | 67.7% | 0.0% (0) |
| 0.75 | 67.7% | 0.0% (0) |
| 0.80 | 67.7% | 0.0% (0) |
| 0.85 | 67.7% | 0.0% (0) |
| 0.90 | 37.3% | 0.0% (0) |
| 0.95 | 0.0% | n/a (0) |

## Why documents went to review

Share of the 97 reviewed documents that carry each reason (a document can carry several).

| Reason | Docs |
|---|---:|
| low confidence in buyer | 38.1% |
| note: line items parsed from free text | 38.1% |
| check instruction_like_text failed: document contains text addressed to an AI system; a human must review | 13.4% |
| low confidence in currency | 12.4% |
| low confidence in po_date | 12.4% |
| check line_totals failed: lines sum to N, document total is N | 11.3% |
| line N: catalog match description … | 11.3% |
| low confidence in delivery_date | 10.3% |
| check count_quantities failed: line N has a fractional quantity of a count unit | 5.2% |
| line N: catalog match exact …: sizes in the description differ from the catalog item | 2.1% |
| line N: catalog match description …: description is close to several items | 1.0% |
