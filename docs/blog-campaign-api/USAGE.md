# Blog Campaign API

Single endpoint for blog sites to create engaged-audience campaigns.

## Endpoint

```
POST https://projectjarvis.io/api/mailing/blog-campaign
```

## Authentication

```
X-Admin-Key: <your-api-key>
Content-Type: application/json
```

## Payload

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `sending_domain` | string | Yes | Your blog's sending domain |
| `subject` | string | Yes | Email subject line (supports Liquid tags) |
| `preview_text` | string | No | Preheader text shown in inbox preview |
| `html_content` | string | Yes | Full HTML email content |
| `scheduled_at` | ISO 8601 | No | When to send (defaults to next day 8 AM MT) |

## What the System Handles Automatically

- Audience: Gmail/Microsoft/Yahoo seed lists + 7-day openers + 14-day clickers
- Exclusion: Global suppression list
- ISP quotas: Unlimited (sends to entire audience)
- Throttling: 15-minute interval waves across 8-hour window
- ISPs: gmail, yahoo, microsoft, apple, comcast, att, cox, charter

## Sending Domains

| Blog | `sending_domain` |
|------|-----------------|
| Discount Blog | `em.discountblog.com` |
| Quiz Fiesta | `em.quizfiesta.com` |
| History Thinking | `em.historythinking.com` |
| My Own Health | `em.myownhealth.net` |

## Example: Discount Blog

```bash
curl -X POST https://projectjarvis.io/api/mailing/blog-campaign \
  -H "X-Admin-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "sending_domain": "em.discountblog.com",
    "subject": "{{ first_name | default: \"Deal Hunter\" }}, Top 5 Spring Deals",
    "preview_text": "AirPods Pro 3 hit $199 — lowest price ever",
    "html_content": "<html><body>Your email HTML here</body></html>"
  }'
```

## Example: Quiz Fiesta

```bash
curl -X POST https://projectjarvis.io/api/mailing/blog-campaign \
  -H "X-Admin-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "sending_domain": "em.quizfiesta.com",
    "subject": "{{ first_name | default: \"Quiz Fan\" }}, Can You Beat This Quiz?",
    "preview_text": "Only 12% got all 10 right",
    "html_content": "<html><body>Your email HTML here</body></html>"
  }'
```

## Example: History Thinking

```bash
curl -X POST https://projectjarvis.io/api/mailing/blog-campaign \
  -H "X-Admin-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "sending_domain": "em.historythinking.com",
    "subject": "{{ first_name | default: \"History Enthusiast\" }}, The Battle That Changed Everything",
    "preview_text": "How one decision in 1066 reshaped the world",
    "html_content": "<html><body>Your email HTML here</body></html>"
  }'
```

## Example: My Own Health

```bash
curl -X POST https://projectjarvis.io/api/mailing/blog-campaign \
  -H "X-Admin-Key: YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "sending_domain": "em.myownhealth.net",
    "subject": "{{ first_name | default: \"Health Seeker\" }}, 5 Morning Habits That Change Everything",
    "preview_text": "Science-backed routines for better energy",
    "html_content": "<html><body>Your email HTML here</body></html>"
  }'
```

## Response (201 Created)

```json
{
  "campaign_id": "uuid",
  "name": "03262026 - Discount Blog - Engaged Audience",
  "status": "scheduled",
  "sends_at": "2026-03-27T15:00:00Z",
  "total_audience": 3500,
  "target_isps": ["gmail", "yahoo", "microsoft", "apple", "comcast", "att", "cox", "charter"],
  "isp_plans": [...],
  "per_isp_selected": {"gmail": 500, "yahoo": 1200, ...}
}
```
