.PHONY: build run clean

TARGET = clone_demo

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

run: build
	# Requires no special privileges on Linux 5.15+ with user namespaces enabled
	# If you get EPERM, run: sudo ./$(TARGET)
	./$(TARGET)

clean:
	rm -f $(TARGET)
