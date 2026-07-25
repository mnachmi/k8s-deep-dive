.PHONY: build run run-pid run-self clean

TARGET = cgroup-stats

build:
	go build -o $(TARGET) .

run: build
	./$(TARGET) /sys/fs/cgroup/user.slice

run-pid: build
	./$(TARGET) --pid $(PID)

run-self: build
	./$(TARGET) --pid $$PPID

clean:
	rm -f $(TARGET)
