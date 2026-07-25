.PHONY: build run-uts run-net run-pid clean

TARGET = mini_unshare

build:
	gcc -Wall -Wextra -Werror -o $(TARGET) $(TARGET).c

# Demo 1: new UTS namespace — hostname change does not affect host
run-uts: build
	sudo ./$(TARGET) --uts -- sh -c 'hostname container-demo && hostname'

# Demo 2: new network namespace — only loopback visible
run-net: build
	sudo ./$(TARGET) --net -- ip addr

# Demo 3: new PID + mount namespace — ps shows only our processes
run-pid: build
	sudo ./$(TARGET) --pid --mount -- ps aux

clean:
	rm -f $(TARGET)
