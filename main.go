package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/windows"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 全局 map 用于存储卡牌 ID（去重）以及 mutex 保护并发操作
var (
	cardIDsMap   = make(map[uint32]bool)
	cardIDsMutex sync.Mutex
)

// Combo 结构体：用于存储可能的组合
type Combo struct {
	Name string   // 组合名字
	CIDs []uint32 // 需要满足的所有 CID
	File string   // 文件名
}

// isAdmin：检测当前进程是否具有管理员权限
func isAdmin() bool {
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0") // 只有管理员可以访问物理驱动
	return err == nil
}

// runAsAdmin：以管理员权限重新启动当前程序
func runAsAdmin() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("无法获取可执行文件路径:", err)
		return
	}

	cmd := exec.Command("powershell", "Start-Process", exe, "-Verb", "RunAs")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err = cmd.Run()
	if err != nil {
		fmt.Println("无法以管理员权限运行:", err)
	}
}

// getProcessID：通过进程名获取 PID
func getProcessID(processName string) (uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)

	var procEntry windows.ProcessEntry32
	procEntry.Size = uint32(unsafe.Sizeof(procEntry))

	if err := windows.Process32First(snapshot, &procEntry); err != nil {
		return 0, err
	}
	for {
		process := syscall.UTF16ToString(procEntry.ExeFile[:])
		if process == processName {
			return procEntry.ProcessID, nil
		}
		if err := windows.Process32Next(snapshot, &procEntry); err != nil {
			break
		}
	}
	return 0, fmt.Errorf("进程未找到: %s", processName)
}

// getModuleBaseAddress：获取指定进程中指定模块的基地址
func getModuleBaseAddress(pid uint32, moduleName string) (uintptr, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, fmt.Errorf("无法打开进程: %v", err)
	}
	defer windows.CloseHandle(handle)

	var hMods [1024]windows.Handle
	var cbNeeded uint32
	if err := windows.EnumProcessModules(handle, &hMods[0], uint32(unsafe.Sizeof(hMods)), &cbNeeded); err != nil {
		return 0, fmt.Errorf("无法枚举进程模块: %v", err)
	}

	numModules := cbNeeded / uint32(unsafe.Sizeof(hMods[0]))
	for i := uint32(0); i < numModules; i++ {
		var modName [windows.MAX_PATH]uint16
		if err := windows.GetModuleBaseName(handle, hMods[i], &modName[0], windows.MAX_PATH); err == nil {
			mod := syscall.UTF16ToString(modName[:])
			if mod == moduleName {
				return uintptr(hMods[i]), nil
			}
		}
	}

	return 0, fmt.Errorf("未找到模块: %s", moduleName)
}

// readMemory：从指定进程的指定地址读取 size 大小的内存字节
func readMemory(processHandle windows.Handle, address uintptr, size int) ([]byte, error) {
	data := make([]byte, size)
	var bytesRead uintptr
	err := windows.ReadProcessMemory(processHandle, address, &data[0], uintptr(size), &bytesRead)
	if err != nil {
		return nil, fmt.Errorf("读取内存失败: %v", err)
	}
	return data, nil
}

// 全局的多级指针偏移表
var offsets = []uintptr{0xB8, 0x0, 0x50, 0x40, 0xC8, 0x150, 0x21C}

// resolvePointer：多级指针解析
func resolvePointer(processHandle windows.Handle, baseAddress uintptr, offsets []uintptr) (uintptr, error) {
	currentAddress := baseAddress
	for _, offset := range offsets {
		data, err := readMemory(processHandle, currentAddress, 8)
		if err != nil {
			return 0, fmt.Errorf("读取地址 0x%X 失败: %v", currentAddress, err)
		}
		parsedPtr := binary.LittleEndian.Uint64(data)
		currentAddress = uintptr(parsedPtr) + offset
	}
	return currentAddress, nil
}

// retryResolvePointer：添加重试机制，直到成功读取最终地址
func retryResolvePointer(processHandle windows.Handle, baseAddress uintptr, offsets []uintptr) uintptr {
	var lastErrorTime time.Time
	for {
		finalAddress, err := resolvePointer(processHandle, baseAddress, offsets)
		if err == nil {
			return finalAddress
		}
		if time.Since(lastErrorTime) >= 5*time.Second {
			log.Printf("多级指针解析失败: %v，重试中...", err)
			lastErrorTime = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// initMemoryMonitor：初始化内存监控，返回打开的进程句柄和目标指针地址（初始地址，不是最终解析地址）
func initMemoryMonitor() (windows.Handle, uintptr, error) {
	// 如果当前不是管理员权限，先以管理员权限重启
	if !isAdmin() {
		fmt.Println("请求管理员权限...")
		runAsAdmin()
		return 0, 0, fmt.Errorf("请以管理员权限运行程序")
	}
	fmt.Println("以管理员权限运行成功！")

	// 目标进程与模块
	processName := "masterduel.exe"
	moduleName := "GameAssembly.dll"
	// 新的基址偏移
	offset := uintptr(0x02E13350)

	// 获取进程 ID
	pid, err := getProcessID(processName)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("找到进程 %s, PID: %d\n", processName, pid)

	// 获取模块基地址
	baseAddress, err := getModuleBaseAddress(pid, moduleName)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("模块 %s 基地址: 0x%X\n", moduleName, baseAddress)

	// 计算多级指针起始地址
	targetAddress := baseAddress + offset
	fmt.Printf("初始目标地址: 0x%X\n", targetAddress)

	// 打开进程
	processHandle, err := windows.OpenProcess(windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, 0, fmt.Errorf("无法打开进程: %v", err)
	}

	return processHandle, targetAddress, nil
}

// monitorCards：持续监控卡牌 ID，每次循环都重新解析最终地址
func monitorCards(processHandle windows.Handle, targetAddress uintptr) {
	fmt.Println("开始持续监控卡牌 ID 的变化……")
	var lastErrorTime time.Time
	for {
		// 每次循环重新获取最终解析地址
		finalAddress := retryResolvePointer(processHandle, targetAddress, offsets)
		data, err := readMemory(processHandle, finalAddress, 4)
		if err != nil {
			if time.Since(lastErrorTime) >= 5*time.Second {
				log.Printf("读取内存失败: %v", err)
				lastErrorTime = time.Now()
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		currentID := binary.LittleEndian.Uint32(data)
		if currentID != 0 {
			cardIDsMutex.Lock()
			if !cardIDsMap[currentID] {
				cardIDsMap[currentID] = true
				fmt.Printf("检测到新的卡牌 ID: %d\n", currentID)
			}
			cardIDsMutex.Unlock()
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// containsAll：判断 cids 是否包含 comboTargets 中所有元素
func containsAll(cids []uint32, comboTargets []uint32) bool {
	cidMap := make(map[uint32]bool, len(cids))
	for _, cid := range cids {
		cidMap[cid] = true
	}
	for _, t := range comboTargets {
		if !cidMap[t] {
			return false
		}
	}
	return true
}

// study：根据收集到的所有卡牌 ID，检测是否满足某些特定组合
func study(cids []uint32) {
	// 示例：随意定义一些组合，用于演示
	combos := []Combo{
		{
			Name: "派兵+烙印融合",
			CIDs: []uint32{15057, 17066},
			File: "./Branded Fusion/派兵+烙印融合.txt",
		},
		{
			Name: "派兵+阿尔贝",
			CIDs: []uint32{15057, 16195},
			File: "./Branded Fusion/派兵+阿尔贝.txt",
		},
		{
			Name: "阿尔贝+阿不思",
			CIDs: []uint32{16195, 15245},
			File: "./Branded Fusion/阿尔贝+阿不思.txt",
		},
		{
			Name: "烙印融合+阿尔贝",
			CIDs: []uint32{17066, 16195},
			File: "./Branded Fusion/烙印融合+阿尔贝.txt",
		},
		{
			Name: "气炎+黑衣龙",
			CIDs: []uint32{16541, 16197},
			File: "./Branded Fusion/气炎+黑衣龙.txt",
		},
		{
			Name: "气炎+金龙",
			CIDs: []uint32{16541, 17765},
			File: "./Branded Fusion/气炎+金龙.txt",
		},
		{
			Name: "气炎+萨隆",
			CIDs: []uint32{16541, 17763},
			File: "./Branded Fusion/气炎+萨隆.txt",
		},
		{
			Name: "气炎+铁兽鸟",
			CIDs: []uint32{16541, 17062},
			File: "./Branded Fusion/气炎+铁兽鸟.txt",
		},
		{
			Name: "烙印融合+金龙（防陨）",
			CIDs: []uint32{17066, 17765},
			File: "./Branded Fusion/烙融+金龙（防陨）.txt",
		},
		{
			Name: "龙魔导守护者+金龙（防陨）",
			CIDs: []uint32{13689, 17765},
			File: "./Branded Fusion/烙融+金龙（防陨）.txt",
		},
		{
			Name: "阿尔贝+金龙（玛格）",
			CIDs: []uint32{16195, 17765},
			File: "./Branded Fusion/阿尔贝+金龙（玛格）.txt",
		},
		{
			Name: "阿尔贝+金龙（萨隆）",
			CIDs: []uint32{16195, 17765},
			File: "./Branded Fusion/阿尔贝+金龙（萨隆）.txt",
		},
		{
			Name: "烙融（冰剑）",
			CIDs: []uint32{17066},
			File: "./Branded Fusion/烙融（冰剑）.txt",
		},
		{
			Name: "烙融（金龙+木偶）",
			CIDs: []uint32{17066},
			File: "./Branded Fusion/烙融（金龙+木偶）.txt",
		},
		{
			Name: "导圣+金龙",
			CIDs: []uint32{18474, 17765},
			File: "./Branded Fusion/导圣+金龙.txt",
		},
	}

	// 找出哪些组合被满足
	var available []Combo
	for _, combo := range combos {
		if containsAll(cids, combo.CIDs) {
			available = append(available, combo)
		}
	}

	if len(available) == 0 {
		fmt.Println("当前没有满足任何组合条件。")
		return
	}

	fmt.Println("检测到以下组合满足条件：")
	for i, combo := range available {
		fmt.Printf("%d) %s\n", i+1, combo.Name)
	}

	fmt.Println("请输入数字序号 (1~9)，查看对应文件内容，或其他任意键退出。")
	reader := bufio.NewReader(os.Stdin)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)

	choice, err := strconv.Atoi(input)
	if err != nil || choice < 1 || choice > len(available) {
		fmt.Println("输入无效或序号超范围，退出。")
		return
	}

	selectedCombo := available[choice-1]
	data, err := ioutil.ReadFile(selectedCombo.File)
	if err != nil {
		log.Printf("读取文件 [%s] 出错: %v\n", selectedCombo.File, err)
		return
	}
	fmt.Printf("文件 [%s] 内容如下：\n", selectedCombo.File)
	fmt.Println(string(data))
}

func main() {
	// 初始化内存监控，获得进程句柄和目标指针地址
	processHandle, targetAddress, err := initMemoryMonitor()
	if err != nil {
		log.Fatal(err)
	}
	// 注意：这里不在 main 中关闭 processHandle，因为 monitorCards 需要持续使用
	go monitorCards(processHandle, targetAddress)

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("输入 r 执行组合检测；输入 quit 退出程序；其他任意键无效。")
		fmt.Print("你的选择: ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		switch strings.ToLower(input) {
		case "r", "":
			// 获取当前收集到的卡牌 ID，并清空全局 map
			cardIDsMutex.Lock()
			var cids []uint32
			for id := range cardIDsMap {
				cids = append(cids, id)
			}
			cardIDsMap = make(map[uint32]bool)
			cardIDsMutex.Unlock()

			fmt.Println(">>> 开始检测组合……")
			study(cids)
			fmt.Println("组合检测完毕，卡牌 ID 数组已清空，继续监控卡牌 ID。")
		case "quit":
			fmt.Println("用户选择退出，程序结束。")
			windows.CloseHandle(processHandle)
			return
		default:
			fmt.Println("无效输入，请重新选择。")
		}
	}
}
