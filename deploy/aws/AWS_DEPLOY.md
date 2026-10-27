# Deploying to AWS with an Application Load Balancer

**Time:** about 2 hours the first time. **Cost:** a few cents to under $1 per day while running.
Check what your account's free tier covers in **Billing → Free tier**, and **delete everything after your submission** (Step 9).

## What we build

```
                Internet
                   │
        ┌──────────▼──────────┐
        │ Application Load    │  public URL, health checks,
        │ Balancer (ALB)      │  spreads traffic across instances
        └─────┬─────────┬─────┘
              │         │
       ┌──────▼──┐ ┌────▼────┐
       │ EC2 api-1│ │ EC2 api-2│   Go API (stateless) + shop page
       └──────┬──┘ └────┬────┘
              └────┬────┘
            ┌──────▼───────┐
            │ EC2 data     │   Redis (stock tokens + queue)
            │              │   Postgres (orders)  +  Go workers
            └──────────────┘
```

| Local (Docker) | AWS (this guide) | Full production version (in report) |
|---|---|---|
| NGINX | **Application Load Balancer** | ALB + AWS WAF (bot + rate rules) |
| 3 API containers | **2 EC2 instances** | ECS/EKS with auto scaling |
| Redis container | Redis on EC2 | ElastiCache for Redis |
| Postgres container | Postgres on EC2 | RDS PostgreSQL (Multi-AZ) |
| NGINX serves the page | API serves the page | S3 + CloudFront CDN |

---

## Step 0: Safety first (5 min)

1. AWS Console → search **Billing** → **Budgets** → **Create budget** → *Zero spend budget* or a **$5 monthly budget** with your email. You get an email before any surprise.
2. Pick **one region** for everything (top right), for example **Asia Pacific (Mumbai) ap-south-1**.

## Step 1: Put the code on GitHub (10 min)

1. Create a **public** repo named `flash-sale-system` on github.com/unnathivinod.
2. In the project folder (PowerShell):
   ```powershell
   git init
   git add .
   git commit -m "Flash sale system"
   git branch -M main
   git remote add origin https://github.com/unnathivinod/flash-sale-system.git
   git push -u origin main
   ```

## Step 2: Security groups (10 min)

EC2 → **Security Groups** → **Create security group** (same VPC, the default one, for all three).

| Name | Inbound rule | Source |
|---|---|---|
| `alb-sg` | HTTP, port 80 | Anywhere-IPv4 `0.0.0.0/0` |
| `api-sg` | Custom TCP, port 8080 | `alb-sg` (pick the group, not an IP) |
| `api-sg` | SSH, port 22 | My IP |
| `data-sg` | Custom TCP, port 6379 | `api-sg` |
| `data-sg` | Custom TCP, port 5432 | `api-sg` |
| `data-sg` | SSH, port 22 | My IP |

This is a key talking point: **only the load balancer is public**. The APIs accept traffic only from the ALB, and the database only from the APIs.

## Step 3: Launch 3 EC2 instances (15 min)

EC2 → **Launch instance**, three times:

| Setting | data | api-1, api-2 |
|---|---|---|
| Name | `flash-data` | `flash-api-1`, `flash-api-2` |
| AMI | Amazon Linux 2023 | Amazon Linux 2023 |
| Type | free-tier eligible (`t3.micro` or `t2.micro`) | same |
| Key pair | create one (`flash-key`, .pem), reuse it | same |
| Security group | `data-sg` | `api-sg` |
| Subnet | any | **put api-1 and api-2 in different Availability Zones** |
| User data | paste `deploy/aws/user-data.sh` (fix the GitHub URL) | same |

Wait ~3 minutes after launch for the user-data setup to finish.

## Step 4: Start the data instance (10 min)

Connect: select `flash-data` → **Connect** → **EC2 Instance Connect** → Connect (browser terminal, no key needed).

```bash
cd ~/flash-sale-system
cp deploy/aws/.env.example deploy/aws/.env
nano deploy/aws/.env        # set the passwords; DATA_HOST is not needed here
docker compose -f deploy/aws/docker-compose.data.yml --env-file deploy/aws/.env up -d --build
docker compose -f deploy/aws/docker-compose.data.yml ps      # all should be "running"/"healthy"
```

Note the **Private IPv4 address** of `flash-data` from the EC2 console (looks like `172.31.x.x`).

## Step 5: Start both API instances (10 min each)

On `flash-api-1`, then the same on `flash-api-2`:

```bash
cd ~/flash-sale-system
cp deploy/aws/.env.example deploy/aws/.env
nano deploy/aws/.env        # SAME passwords as data; DATA_HOST = data's private IP
docker compose -f deploy/aws/docker-compose.api.yml --env-file deploy/aws/.env up -d --build
curl localhost:8080/healthz # should print: ok
```

## Step 6: Target group (5 min)

EC2 → **Target Groups** → **Create target group**
- Type: **Instances** · Name: `flash-api-tg` · Protocol **HTTP**, port **8080**
- Health check path: **`/healthz`** · Advanced: healthy threshold 2, interval 10s
- Next → select `flash-api-1` and `flash-api-2` → **Include as pending below** → Create

## Step 7: Application Load Balancer (10 min)

EC2 → **Load Balancers** → **Create** → **Application Load Balancer**
- Name `flash-alb` · **Internet-facing** · IPv4
- Network mapping: select **at least two Availability Zones** (the ones your API instances are in)
- Security group: **`alb-sg`** only (remove default)
- Listener HTTP:80 → forward to **`flash-api-tg`**
- Create, wait ~3 minutes until State = **Active**

Then Target Groups → `flash-api-tg` → **Targets**: both should say **healthy**.

Open the ALB's **DNS name** (e.g. `flash-alb-123.ap-south-1.elb.amazonaws.com`) in your browser. That's your live shop. `/stats.html` is the monitor.

**Screenshots for the report:** ALB overview, listener, target group with 2 healthy targets, security groups, live page.

## Step 8: Load test through the ALB (10 min)

From your laptop (PowerShell, project folder). Home internet limits how hard you can push, so use a moderate burst:

```powershell
$ALB="http://YOUR-ALB-DNS-NAME"
docker run --rm -v "${PWD}/loadtest:/scripts" grafana/k6 run `
  -e BASE=$ALB -e ADMIN=YourAdminToken -e LOADTEST_KEY=YourLoadtestKey `
  -e VUS=300 -e REQUESTS=5000 /scripts/flash-sale.js
```

Expected: exactly **500 confirmed**, all other requests get a clean 409, `http_req_failed` 0%.

Also show **failover**: stop one API (`docker compose -f deploy/aws/docker-compose.api.yml down` on api-2). The ALB marks it unhealthy and the site keeps working on api-1.

## Step 9: Delete everything (10 min), after submission

In this order: **Load balancer** → **Target group** → **Terminate all 3 instances** → Security groups → Key pair. Check **Billing** the next day.

---

## Troubleshooting

| Problem | Fix |
|---|---|
| Targets "unhealthy" | `api-sg` must allow 8080 **from `alb-sg`**; check `curl localhost:8080/healthz` on the instance |
| API logs say "redis/postgres not reachable" | `data-sg` must allow 6379/5432 **from `api-sg`**; `DATA_HOST` must be the **private** IP; passwords identical |
| `docker: command not found` | user-data still running or failed: `sudo cat /var/log/cloud-init-output.log` |
| Build killed / out of memory | swap missing: `free -h`; rerun the swap lines from user-data.sh |
| 502 from ALB | API container crashed: `docker compose -f deploy/aws/docker-compose.api.yml logs` |
