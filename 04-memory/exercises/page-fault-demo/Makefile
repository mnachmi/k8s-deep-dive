.PHONY: build run clean

TARGET = page_fault_demo

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

run: build
	./$(TARGET)

clean:
	rm -f $(TARGET)
