.PHONY: build run run-pid clean

TARGET = namespace-inspector

build:
	go build -o $(TARGET) .

# Scan all processes and show shared namespace groups
run: build
	./$(TARGET)

# Show all namespaces for a specific PID (pass PID=<n>)
run-pid: build
	./$(TARGET) $(PID)

clean:
	rm -f $(TARGET)
