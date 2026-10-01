package approval

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Just enough of the Windows Runtime to show and remove toasts, called
// through combase.dll and the interfaces' vtables, with no cgo and no
// dependencies. Vtable slots are the IUnknown/IInspectable six plus the
// method's position in the interface (checked against windows.ui.notifications.h).

var (
	combase                    = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = combase.NewProc("RoInitialize")
	procRoGetActivationFactory = combase.NewProc("RoGetActivationFactory")
	procRoActivateInstance     = combase.NewProc("RoActivateInstance")
	procWindowsCreateString    = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString    = combase.NewProc("WindowsDeleteString")
)

var (
	iidXmlDocumentIO       = windows.GUID{Data1: 0x6cd0e74e, Data2: 0xee65, Data3: 0x4489, Data4: [8]byte{0x9e, 0xbf, 0xca, 0x43, 0xe8, 0x7b, 0xa6, 0x37}}
	iidToastFactory        = windows.GUID{Data1: 0x04124b20, Data2: 0x82c6, Data3: 0x4229, Data4: [8]byte{0xb1, 0x09, 0xfd, 0x9e, 0xd4, 0x66, 0x2b, 0x53}}
	iidToast2              = windows.GUID{Data1: 0x9dfb9fd1, Data2: 0x143a, Data3: 0x490e, Data4: [8]byte{0x90, 0xbf, 0xb9, 0xfb, 0xa7, 0x13, 0x2d, 0xe7}}
	iidManagerStatics      = windows.GUID{Data1: 0x50ac103f, Data2: 0xd235, Data3: 0x4598, Data4: [8]byte{0xbb, 0xef, 0x98, 0xfe, 0x4d, 0x1a, 0x3a, 0xd4}}
	iidManagerStatics2     = windows.GUID{Data1: 0x7ab93c52, Data2: 0x0e48, Data3: 0x4750, Data4: [8]byte{0xba, 0x9d, 0x1a, 0x41, 0x13, 0x98, 0x18, 0x47}}
	iidPropertyValueStatic = windows.GUID{Data1: 0x629bdbc8, Data2: 0xd932, Data3: 0x4ff4, Data4: [8]byte{0x96, 0xb9, 0x8d, 0x96, 0xc5, 0xc1, 0xe8, 0x58}}
	iidRefDateTime         = windows.GUID{Data1: 0x5541d8a7, Data2: 0x497c, Data3: 0x5aa4, Data4: [8]byte{0x86, 0xfc, 0x77, 0x13, 0xad, 0xbf, 0x2a, 0x2c}}
)

// Vtable slots used below.
const (
	slotQueryInterface = 0
	slotRelease        = 2

	slotLoadXml                = 6  // IXmlDocumentIO
	slotCreateToast            = 6  // IToastNotificationFactory
	slotToastExpiration        = 7  // IToastNotification.put_ExpirationTime
	slotToastTag               = 6  // IToastNotification2.put_Tag
	slotToastGroup             = 8  // IToastNotification2.put_Group
	slotCreateNotifierWithID   = 7  // IToastNotificationManagerStatics
	slotManagerHistory         = 6  // IToastNotificationManagerStatics2.get_History
	slotNotifierShow           = 6  // IToastNotifier
	slotNotifierSetting        = 8  // IToastNotifier.get_Setting
	slotHistoryRemoveGroupedID = 8  // IToastNotificationHistory.RemoveGroupedTagWithId
	slotHistoryRemoveGroupID   = 7  // IToastNotificationHistory.RemoveGroupWithId
	slotHistoryClearWithID     = 12 // IToastNotificationHistory.ClearWithId
	slotCreateDateTime         = 21 // IPropertyValueStatics.CreateDateTime
)

// object is a WinRT interface pointer.
type object struct{ p unsafe.Pointer }

func (o object) call(slot int, args ...uintptr) error {
	vtbl := *(**[64]uintptr)(o.p)
	r, _, _ := syscall.SyscallN(vtbl[slot], append([]uintptr{uintptr(o.p)}, args...)...)
	if int32(r) < 0 {
		return fmt.Errorf("WinRT call (slot %d) failed: %w", slot, syscall.Errno(uint32(r)))
	}
	return nil
}

// callObj calls a method whose last argument is an interface out-pointer.
// The result slot lives on the heap: Go stacks move, and the OS holds the address.
func (o object) callObj(slot int, args ...uintptr) (object, error) {
	out := new(unsafe.Pointer)
	err := o.call(slot, append(args, uintptr(unsafe.Pointer(out)))...)
	return object{*out}, err
}

func (o object) release() {
	if o.p != nil {
		o.call(slotRelease)
	}
}

func (o object) query(iid *windows.GUID) (object, error) {
	return o.callObj(slotQueryInterface, uintptr(unsafe.Pointer(iid)))
}

// hstring is a WinRT string; free with delete.
type hstring uintptr

func newHString(s string) (hstring, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	h := new(hstring) // result slots live on the heap: Go stacks move, and the OS holds the address
	r, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(h)))
	runtime.KeepAlive(u)
	if int32(r) < 0 {
		return 0, fmt.Errorf("WindowsCreateString: %w", syscall.Errno(uint32(r)))
	}
	return *h, nil
}

func (h hstring) delete() { procWindowsDeleteString.Call(uintptr(h)) }

// hstrings creates several strings at once; free them all with the returned func.
func hstrings(vals ...string) ([]hstring, func(), error) {
	out := make([]hstring, 0, len(vals))
	free := func() {
		for _, h := range out {
			h.delete()
		}
	}
	for _, v := range vals {
		h, err := newHString(v)
		if err != nil {
			free()
			return nil, nil, err
		}
		out = append(out, h)
	}
	return out, free, nil
}

func activationFactory(class string, iid *windows.GUID) (object, error) {
	h, err := newHString(class)
	if err != nil {
		return object{}, err
	}
	defer h.delete()
	out := new(unsafe.Pointer)
	r, _, _ := procRoGetActivationFactory.Call(uintptr(h), uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(out)))
	runtime.KeepAlive(iid)
	if int32(r) < 0 {
		return object{}, fmt.Errorf("RoGetActivationFactory(%s): %w", class, syscall.Errno(uint32(r)))
	}
	return object{*out}, nil
}

func activateInstance(class string) (object, error) {
	h, err := newHString(class)
	if err != nil {
		return object{}, err
	}
	defer h.delete()
	out := new(unsafe.Pointer)
	r, _, _ := procRoActivateInstance.Call(uintptr(h), uintptr(unsafe.Pointer(out)))
	if int32(r) < 0 {
		return object{}, fmt.Errorf("RoActivateInstance(%s): %w", class, syscall.Errno(uint32(r)))
	}
	return object{*out}, nil
}

// runtimeThread runs WinRT calls on one OS thread that joined the
// multithreaded apartment, so no call depends on which goroutine made it.
var runtimeThread struct {
	once sync.Once
	jobs chan func()
	err  error
}

func onRuntimeThread(fn func() error) error {
	runtimeThread.once.Do(func() {
		runtimeThread.jobs = make(chan func())
		ready := make(chan error, 1)
		go func() {
			runtime.LockOSThread() // for the life of the process
			const roInitMultithreaded = 1
			r, _, _ := procRoInitialize.Call(roInitMultithreaded)
			// S_OK, S_FALSE (already initialised) and RPC_E_CHANGED_MODE (another
			// apartment model; WinRT calls still work from this thread) are all usable.
			if int32(r) < 0 && uint32(r) != 0x80010106 {
				ready <- fmt.Errorf("RoInitialize: %w", syscall.Errno(uint32(r)))
				return
			}
			ready <- nil
			for job := range runtimeThread.jobs {
				job()
			}
		}()
		runtimeThread.err = <-ready
	})
	if runtimeThread.err != nil {
		return runtimeThread.err
	}
	done := make(chan error, 1)
	runtimeThread.jobs <- func() { done <- fn() }
	return <-done
}

// Notification settings (NotificationSetting enum).
const settingEnabled = 0

var settingNames = map[int32]string{
	1: "disabled for messh in Settings > System > Notifications",
	2: "disabled by the user",
	3: "disabled by group policy",
	4: "disabled by the app manifest",
}

// showToast shows toast XML under the given tag and group, expiring at expires.
func showToast(aumid, xml, tag, group string, expires time.Time) error {
	return onRuntimeThread(func() error {
		doc, err := activateInstance("Windows.Data.Xml.Dom.XmlDocument")
		if err != nil {
			return err
		}
		defer doc.release()
		io, err := doc.query(&iidXmlDocumentIO)
		if err != nil {
			return err
		}
		defer io.release()
		hs, free, err := hstrings(xml, tag, group, aumid)
		if err != nil {
			return err
		}
		defer free()
		if err := io.call(slotLoadXml, uintptr(hs[0])); err != nil {
			return fmt.Errorf("toast XML rejected: %w", err)
		}

		factory, err := activationFactory("Windows.UI.Notifications.ToastNotification", &iidToastFactory)
		if err != nil {
			return err
		}
		defer factory.release()
		toast, err := factory.callObj(slotCreateToast, uintptr(doc.p))
		if err != nil {
			return err
		}
		defer toast.release()

		toast2, err := toast.query(&iidToast2)
		if err != nil {
			return err
		}
		defer toast2.release()
		if err := toast2.call(slotToastTag, uintptr(hs[1])); err != nil {
			return fmt.Errorf("set toast tag: %w", err)
		}
		if err := toast2.call(slotToastGroup, uintptr(hs[2])); err != nil {
			return fmt.Errorf("set toast group: %w", err)
		}

		if !expires.IsZero() {
			if err := setExpiration(toast, expires); err != nil {
				return err
			}
		}

		mgr, err := activationFactory("Windows.UI.Notifications.ToastNotificationManager", &iidManagerStatics)
		if err != nil {
			return err
		}
		defer mgr.release()
		notifier, err := mgr.callObj(slotCreateNotifierWithID, uintptr(hs[3]))
		if err != nil {
			return err
		}
		defer notifier.release()
		// Windows cannot always report the setting for an app it has not shown
		// anything for yet; Show below then decides.
		setting := new(int32)
		if err := notifier.call(slotNotifierSetting, uintptr(unsafe.Pointer(setting))); err == nil && *setting != settingEnabled {
			why := settingNames[*setting]
			if why == "" {
				why = fmt.Sprintf("setting %d", *setting)
			}
			return fmt.Errorf("Windows notifications are %s", why)
		}
		return notifier.call(slotNotifierShow, uintptr(toast.p))
	})
}

// setExpiration makes Windows drop the toast at t even if the node died.
func setExpiration(toast object, t time.Time) error {
	pv, err := activationFactory("Windows.Foundation.PropertyValue", &iidPropertyValueStatic)
	if err != nil {
		return err
	}
	defer pv.release()
	// DateTime counts 100 ns ticks since 1601-01-01 UTC and is passed by value.
	ticks := t.UnixNano()/100 + 116444736000000000
	boxed, err := pv.callObj(slotCreateDateTime, uintptr(ticks))
	if err != nil {
		return err
	}
	defer boxed.release()
	ref, err := boxed.query(&iidRefDateTime)
	if err != nil {
		return err
	}
	defer ref.release()
	return toast.call(slotToastExpiration, uintptr(ref.p))
}

// history runs fn on the toast history of the messh app.
func history(fn func(h object) error) error {
	return onRuntimeThread(func() error {
		mgr, err := activationFactory("Windows.UI.Notifications.ToastNotificationManager", &iidManagerStatics2)
		if err != nil {
			return err
		}
		defer mgr.release()
		h, err := mgr.callObj(slotManagerHistory)
		if err != nil {
			return err
		}
		defer h.release()
		return fn(h)
	})
}

// hideToast removes the toast with this tag from the screen and Action Center.
func hideToast(aumid, tag, group string) error {
	return history(func(h object) error {
		hs, free, err := hstrings(tag, group, aumid)
		if err != nil {
			return err
		}
		defer free()
		return h.call(slotHistoryRemoveGroupedID, uintptr(hs[0]), uintptr(hs[1]), uintptr(hs[2]))
	})
}

// hideGroup removes every toast of a group.
func hideGroup(aumid, group string) error {
	return history(func(h object) error {
		hs, free, err := hstrings(group, aumid)
		if err != nil {
			return err
		}
		defer free()
		return h.call(slotHistoryRemoveGroupID, uintptr(hs[0]), uintptr(hs[1]))
	})
}

// clearToasts removes every toast of the app.
func clearToasts(aumid string) error {
	return history(func(h object) error {
		hs, free, err := hstrings(aumid)
		if err != nil {
			return err
		}
		defer free()
		return h.call(slotHistoryClearWithID, uintptr(hs[0]))
	})
}
