# Evaluation: rule-based baseline

Produced by `ingest eval -extractor rules -n 1000 -seed 2026 -threshold 0.85`. Synthetic documents only; see `internal/synth` for how they are generated.

Documents: 1000 (chat 119, csv 161, eml 224, pdf 152, txt 195, xlsx 149).

## Routing at threshold 0.85

| Metric | Value |
|---|---|
| Straight-through rate (auto-accepted / all) | 68.8% (688/1000) |
| Error rate among auto-accepted | 0.4% (3/688) |
| Documents fully correct, any route | 84.0% (840/1000) |
| Sent to human review | 312 |

A document is "fully correct" when every header field, every line field and every catalog match equals ground truth.

## Field-level extraction

| Field | Support | Precision | Recall | F1 |
|---|---:|---:|---:|---:|
| doc_type | 1000 | 1.000 | 1.000 | 1.000 |
| buyer | 1000 | 1.000 | 0.881 | 0.937 |
| po_number | 1000 | 1.000 | 1.000 | 1.000 |
| po_date | 846 | 1.000 | 1.000 | 1.000 |
| delivery_date | 848 | 1.000 | 0.882 | 0.937 |
| currency | 870 | 1.000 | 0.945 | 0.972 |
| total | 699 | 1.000 | 0.878 | 0.935 |
| part_number | 3206 | 0.987 | 0.974 | 0.980 |
| description | 3467 | 0.979 | 0.972 | 0.975 |
| quantity | 3467 | 0.975 | 0.968 | 0.971 |
| unit | 3467 | 0.993 | 0.986 | 0.990 |
| unit_price | 2748 | 0.987 | 0.980 | 0.983 |
| catalog_sku | 3467 | 0.983 | 0.977 | 0.980 |

Line fields are aligned by position. Support counts ground-truth values that are present; a wrong value counts as one false positive and one false negative.

## By format

| Format | Docs | Straight-through | Errors among auto-accepted | Fully correct |
|---|---:|---:|---:|---:|
| chat | 119 | 0.0% | n/a | 0.0% |
| csv | 161 | 77.0% | 0.0% | 93.8% |
| eml | 224 | 78.1% | 0.6% | 94.6% |
| pdf | 152 | 78.3% | 0.8% | 94.7% |
| txt | 195 | 78.5% | 0.7% | 94.9% |
| xlsx | 149 | 78.5% | 0.0% | 99.3% |

## Threshold sweep

Re-thresholds the recorded document confidence. Use it to pick the operating point: lower thresholds buy straight-through rate with errors.

| Threshold | Straight-through | Errors among auto-accepted |
|---:|---:|---:|
| 0.50 | 80.1% | 2.7% (22) |
| 0.60 | 69.6% | 0.4% (3) |
| 0.70 | 68.8% | 0.4% (3) |
| 0.75 | 68.8% | 0.4% (3) |
| 0.80 | 68.8% | 0.4% (3) |
| 0.85 | 68.8% | 0.4% (3) |
| 0.90 | 36.4% | 0.5% (2) |
| 0.95 | 0.0% | n/a (0) |

## Why documents went to review

Share of the 312 reviewed documents that carry each reason (a document can carry several).

| Reason | Docs |
|---|---:|
| low confidence in buyer | 37.8% |
| note: line items parsed from free text | 37.8% |
| check line_totals failed: lines sum to N, document total is N | 17.9% |
| line N: catalog match description … | 14.1% |
| low confidence in po_date | 11.5% |
| low confidence in currency | 11.2% |
| check instruction_like_text failed: document contains text addressed to an AI system; a human must review | 9.9% |
| low confidence in delivery_date | 8.3% |
| check count_quantities failed: line N has a fractional quantity of a count unit | 6.7% |
| line N: catalog match exact …: sizes in the description differ from the catalog item | 3.5% |
| line N: catalog match description …: description is close to several items | 1.0% |
| no line items found | 0.3% |
