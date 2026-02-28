# IGNITE Mailing Platform

Enterprise-grade email marketing platform with AI-powered delivery optimization.

## Features

- **Mass Scale Delivery**: 8M+ emails/day capacity
- **Multi-ESP Support**: SparkPost, AWS SES, with failover
- **AI-Powered Optimization**:
  - Optimal send time prediction per subscriber
  - Engagement scoring and segmentation
  - Revenue-driven autonomous sending plans
  - Individualized mailbox intelligence
- **Complete Campaign Management**:
  - List management with double opt-in
  - Segmentation and targeting
  - A/B testing support
  - Template management
- **Real-time Tracking**:
  - Opens, clicks, conversions
  - Bounce and complaint handling
  - Revenue attribution
- **Compliance Ready**:
  - GDPR, CAN-SPAM, CCPA, CASL
  - One-click unsubscribe
  - Data retention policies

## Quick Start

### Prerequisites

- Docker & Docker Compose
- Go 1.22+ (for local development)
- Node.js 20+ (for frontend development)

### Development Setup

1. Clone the repository:
```bash
git clone https://github.com/ignite/mailing-platform.git
cd mailing-platform
```

2. Copy environment file:
```bash
cp .env.example .env
# Edit .env with your credentials
```

3. Start services:
```bash
docker-compose up -d
```

4. Access the application:
- Frontend: http://localhost:3000
- API: http://localhost:8080
- Grafana: http://localhost:3001

### Local Development

**Backend:**
```bash
go mod download
go run cmd/api/main.go
```

**Frontend:**
```bash
cd frontend
npm install
npm run dev
```

**Run Tests:**
```bash
go test ./...
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Load Balancer                            │
└─────────────────────────────────────────────────────────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        │                       │                       │
   ┌────▼────┐            ┌─────▼─────┐           ┌────▼────┐
   │ Frontend│            │    API    │           │ Tracking│
   │ (React) │            │   Server  │           │ Service │
   └─────────┘            └─────┬─────┘           └────┬────┘
                                │                       │
        ┌───────────────────────┼───────────────────────┤
        │                       │                       │
   ┌────▼────┐            ┌─────▼─────┐           ┌────▼────┐
   │PostgreSQL│           │   Redis   │           │  Worker │
   │ Database │           │   Cache   │           │ Service │
   └──────────┘           └───────────┘           └────┬────┘
                                                        │
                                          ┌─────────────┼─────────────┐
                                          │             │             │
                                     ┌────▼────┐  ┌─────▼─────┐ ┌────▼────┐
                                     │SparkPost│  │  AWS SES  │ │ Mailgun │
                                     └─────────┘  └───────────┘ └─────────┘
```

## API Endpoints

### Dashboard
- `GET /api/mailing/dashboard` - Get dashboard statistics

### Lists
- `GET /api/mailing/lists` - List all mailing lists
- `POST /api/mailing/lists` - Create new list
- `GET /api/mailing/lists/:id` - Get list details
- `GET /api/mailing/lists/:id/subscribers` - Get subscribers

### Campaigns
- `GET /api/mailing/campaigns` - List campaigns
- `POST /api/mailing/campaigns` - Create campaign
- `GET /api/mailing/campaigns/:id` - Get campaign details
- `POST /api/mailing/campaigns/:id/send` - Send campaign

### AI Plans
- `GET /api/mailing/sending-plans` - Generate AI sending plans

### Tracking
- `GET /track/open/:data/:sig` - Track email open
- `GET /track/click/:data/:sig` - Track link click
- `GET /track/unsubscribe/:data/:sig` - Handle unsubscribe

### Webhooks
- `POST /webhooks/sparkpost` - SparkPost events
- `POST /webhooks/ses` - AWS SES events

## Configuration

Environment variables are documented in `.env.example`.

## License

Proprietary - All rights reserved.
