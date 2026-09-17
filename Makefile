.PHONY: install deps backend frontend run build deploy clean

install:
	cd frontend && npm install

deps:
	go mod download
	cd frontend && npm install

backend:
	ENV=dev JIRA_API_TOKEN=$${JIRA_API_TOKEN} go run main.go

frontend:
	cd frontend && npm run dev

build-frontend:
	cd frontend && npm run build

run: deps
	@echo "Starting Go backend and React frontend in dev mode..."
	@echo "Backend will run on http://localhost:8082"
	@echo "Frontend will run on http://localhost:3000"
	@make -j2 backend frontend

build: deps
	go generate
	go build -o app main.go

deploy:
	apps-platform app deploy

clean:
	cd frontend && rm -rf node_modules dist
	rm -f app
	go clean
