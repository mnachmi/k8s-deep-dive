.PHONY: build run run-pid run-cgroup clean

TARGET = psi-reader

build:
	go build -o $(TARGET) .

run: build
	./$(TARGET)

run-pid: build
	./$(TARGET) --pid $(PID)

run-cgroup: build
	./$(TARGET) --cgroup $(CGROUP)

clean:
	rm -f $(TARGET)
