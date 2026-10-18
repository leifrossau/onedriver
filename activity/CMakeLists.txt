cmake_minimum_required(VERSION 3.20)
project(onedriver-activity LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

find_package(Qt6 6.5 REQUIRED COMPONENTS Core Gui DBus Qml Quick QuickControls2)
# Kirigami is a runtime dependency of the QML (theme colors and icons).
find_package(KF6Kirigami REQUIRED)
# Optional: anchors the popup above the panel on Wayland. Without it the
# window is placed bottom-right with plain Qt, which only works on X11.
find_package(LayerShellQt QUIET)

qt_standard_project_setup(REQUIRES 6.5)
# Put QML modules under qrc:/qt/qml, the default import path, so the installed
# binary finds its own QML (the build tree only worked through files on disk).
qt_policy(SET QTP0001 NEW)

# everything except main(), shared with the tests
add_library(activity-core STATIC
    src/backend.cpp
    src/backend.h
    src/dbustypes.cpp
    src/dbustypes.h
    src/helpers.cpp
    src/helpers.h
    src/models.cpp
    src/models.h
    src/popup.cpp
    src/popup.h
)
target_include_directories(activity-core PUBLIC src)
target_link_libraries(activity-core PUBLIC Qt6::Core Qt6::Gui Qt6::DBus Qt6::Quick)
if(LayerShellQt_FOUND)
    message(STATUS "LayerShellQt found: the popup will use the layer-shell protocol on Wayland")
    target_compile_definitions(activity-core PUBLIC HAVE_LAYERSHELLQT)
    target_link_libraries(activity-core PUBLIC LayerShellQt::Interface)
    if(LayerShellQt_VERSION VERSION_GREATER_EQUAL 6.6)
        # Window::setScreen: the output of the layer surface (before that it follows QWindow::screen)
        target_compile_definitions(activity-core PUBLIC HAVE_LAYERSHELLQT_SCREEN)
    endif()
else()
    message(STATUS "LayerShellQt not found: the popup will not be anchored on Wayland")
endif()

qt_add_executable(onedriver-activity src/main.cpp)
qt_add_qml_module(onedriver-activity
    URI org.onedriver.Activity
    VERSION 1.0
    QML_FILES qml/Main.qml
)
target_link_libraries(onedriver-activity PRIVATE activity-core Qt6::Qml Qt6::QuickControls2)

include(CTest)
if(BUILD_TESTING)
    find_package(Qt6 REQUIRED COMPONENTS Test)
    add_subdirectory(tests)
endif()

include(GNUInstallDirs)
install(TARGETS onedriver-activity RUNTIME DESTINATION ${CMAKE_INSTALL_BINDIR})
install(FILES ${CMAKE_CURRENT_SOURCE_DIR}/../pkg/resources/onedriver-activity.desktop
        DESTINATION ${CMAKE_INSTALL_DATADIR}/applications)
