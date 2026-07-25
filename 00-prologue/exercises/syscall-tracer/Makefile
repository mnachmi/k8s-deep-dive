.PHONY: build run clean

TARGET = syscall_tracer

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

run: build
	./$(TARGET) /bin/ls /tmp

clean:
	rm -f $(TARGET)
