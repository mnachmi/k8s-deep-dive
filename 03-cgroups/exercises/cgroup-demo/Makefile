.PHONY: build run clean

TARGET = cgroup_demo

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

run: build
	sudo ./$(TARGET)

clean:
	rm -f $(TARGET)
	sudo rmdir /sys/fs/cgroup/cgroup-demo-test 2>/dev/null || true
