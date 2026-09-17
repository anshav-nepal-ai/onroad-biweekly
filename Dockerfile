# Stage 1: Build React frontend
FROM node:20-slim AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# Stage 2: Build Go binary
FROM golang:1.25-bookworm AS go-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY --from=frontend-builder /app/frontend/dist ./frontend/dist
RUN go build -o app .

# Stage 3: Final image — Python runtime + Go binary
FROM python:3.12-slim
WORKDIR /app

COPY requirements.txt ./
# Install into system Python — Go server calls `python3` which resolves to the
# system interpreter in this image, not a venv.
RUN pip install --no-cache-dir -r requirements.txt

COPY --from=go-builder /app/app ./app
COPY refresh.py ./refresh.py

ENV PORT=8080
CMD ["./app"]
