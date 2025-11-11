#Build
FROM golang:1.25-alpine AS builder

#set work directory
WORKDIR /app

#Copy Go modules manifests
COPY go.mod go.sum ./

#Download Go modules
RUN go mod download

#copy the rest
COPY . .

#build
RUN go build -o stocky-api ./cmd/stocky-api/main.go

# run stage
FROM alpine:latest

#working directive
WORKDIR /app

#Copy the binary 
COPY --from=builder /app/stocky-api .

# Copy migrations folder
COPY internal/database/migrations ./internal/database/migrations

# Expose API port
EXPOSE 8080

# Run the API with config file
CMD ["./stocky-api"]
